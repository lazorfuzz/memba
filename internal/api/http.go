package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/lazorfuzz/memba/internal/authz"
	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

type ctxKey int

const principalKey ctxKey = 1

// Metrics: minimal Prometheus text counters (spec §16; OTel wiring is an
// adapter on top of these call sites).
type Metrics struct {
	Queries      atomic.Int64
	Inserts      atomic.Int64
	Proposals    atomic.Int64
	Verifies     atomic.Int64
	ACLDenials   atomic.Int64
	Quarantines  atomic.Int64
	StaleServed  atomic.Int64
	RateLimited  atomic.Int64
}

func (m *Metrics) render(w io.Writer) {
	fmt.Fprintf(w, "memba_query_total %d\n", m.Queries.Load())
	fmt.Fprintf(w, "memba_insert_total %d\n", m.Inserts.Load())
	fmt.Fprintf(w, "memba_proposals_total %d\n", m.Proposals.Load())
	fmt.Fprintf(w, "memba_verifications_total %d\n", m.Verifies.Load())
	fmt.Fprintf(w, "memba_acl_denials_total %d\n", m.ACLDenials.Load())
	fmt.Fprintf(w, "memba_injection_quarantine_total %d\n", m.Quarantines.Load())
	fmt.Fprintf(w, "memba_stale_served_total %d\n", m.StaleServed.Load())
	fmt.Fprintf(w, "memba_rate_limited_total %d\n", m.RateLimited.Load())
}

// Server hosts the /v1 routes.
type Server struct {
	Svc     *Service
	Secret  []byte
	Metrics Metrics
	MaxBody int64
	limiter *rateLimiter
}

func NewServer(svc *Service) (*Server, error) {
	secretEnv := svc.Cfg.Security.TokenSecretEnv
	secret := os.Getenv(secretEnv)
	if secret == "" {
		return nil, fmt.Errorf("%s must be set (HMAC key for bearer tokens)", secretEnv)
	}
	return &Server{
		Svc:     svc,
		Secret:  []byte(secret),
		MaxBody: int64(svc.Cfg.Server.MaxBodyMB) << 20,
		limiter: newRateLimiter(svc.Cfg.Server.RateLimitPerMin),
	}, nil
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Get("/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Svc.Store.Ping(r.Context()); err != nil {
			problem(w, http.StatusServiceUnavailable, "unhealthy", err.Error())
			return
		}
		w.Write([]byte("ok"))
	})
	r.Get("/v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.Metrics.render(w)
	})

	// The curation UI is a static page; its data calls carry bearer tokens.
	r.Get("/ui", s.handleUI)

	r.Group(func(r chi.Router) {
		r.Use(s.authMiddleware)
		r.Use(s.rateLimitMiddleware)
		r.Get("/v1/admin/health", s.handleHealth)
		r.Post("/v1/admin/bootstrap", s.handleBootstrap)
		r.Post("/v1/evidence", s.handleInsert)
		r.Post("/v1/query", s.handleQuery)
		r.Get("/v1/profile", s.handleProfile)
		r.Get("/v1/workspaces/{id}", s.handleWorkspaceManifest)
		r.Get("/v1/workspaces/{id}/archive", s.handleWorkspaceArchive)
		r.Post("/v1/verify", s.handleVerify)
		r.Post("/v1/cards", s.handlePropose)
		r.Get("/v1/cards", s.handleListCards)
		r.Get("/v1/cards/{id}", s.handleGetCard)
		r.Post("/v1/cards/{id}/review", s.handleReview)
		r.Post("/v1/cards/{id}/invalidate", s.handleInvalidate)
		r.Post("/v1/facts", s.handleInsertFact)
		r.Post("/v1/actions", s.handleAction)
		r.Post("/v1/admin/namespaces", s.handleUpsertNamespace)
	})
	return r
}

// --- middleware --------------------------------------------------------------

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			problem(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		p, err := authz.Verify(s.Secret, strings.TrimPrefix(h, "Bearer "))
		if err != nil {
			problem(w, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
			return
		}
		if s.MaxBody > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, s.MaxBody)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

func principalFrom(r *http.Request) memory.Principal {
	p, _ := r.Context().Value(principalKey).(memory.Principal)
	return p
}

// --- handlers ------------------------------------------------------------------

func (s *Server) handleInsert(w http.ResponseWriter, r *http.Request) {
	var req memory.InsertRequest
	if !decode(w, r, &req) {
		return
	}
	p := principalFrom(r)
	resp, err := s.Svc.Ingest.Insert(r.Context(), p, req)
	if err != nil {
		problem(w, http.StatusUnprocessableEntity, "insert failed", err.Error())
		return
	}
	s.Metrics.Inserts.Add(1)
	if resp.Quarantined {
		s.Metrics.Quarantines.Add(1)
	}
	status := http.StatusCreated
	if resp.Deduplicated {
		status = http.StatusOK
	}
	writeJSON(w, status, resp)
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req memory.QueryRequest
	if !decode(w, r, &req) {
		return
	}
	p := principalFrom(r)
	ep, err := s.Svc.Query(r.Context(), p, req)
	if err != nil {
		problem(w, http.StatusInternalServerError, "query failed", err.Error())
		return
	}
	s.Metrics.Queries.Add(1)
	s.Metrics.StaleServed.Add(int64(len(ep.StaleFlagged)))
	writeJSON(w, http.StatusOK, ep)
}

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	level, _ := strconv.Atoi(r.URL.Query().Get("level"))
	body, etag, err := s.Svc.Profile(r.Context(), p, r.URL.Query().Get("namespace"), r.URL.Query().Get("repo"), level)
	if err != nil {
		problem(w, http.StatusInternalServerError, "profile failed", err.Error())
		return
	}
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write([]byte(body))
}

func (s *Server) handleWorkspaceManifest(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	m, _, err := s.Svc.Store.GetWorkspace(r.Context(), p.TenantID, chi.URLParam(r, "id"))
	if err != nil {
		notFound(w) // 403 never distinguishable from 404 (§13)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleWorkspaceArchive(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	_, objectKey, err := s.Svc.Store.GetWorkspace(r.Context(), p.TenantID, chi.URLParam(r, "id"))
	if err != nil {
		notFound(w)
		return
	}
	rc, err := s.Svc.Workspace.Blobs.Get(r.Context(), objectKey)
	if err != nil {
		notFound(w)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/zstd")
	w.Header().Set("Content-Disposition", `attachment; filename=".memworkspace.tar.zst"`)
	io.Copy(w, rc)
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req memory.VerifyRequest
	if !decode(w, r, &req) {
		return
	}
	p := principalFrom(r)
	res, err := s.Svc.Verify(r.Context(), p, req)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			notFound(w)
			return
		}
		problem(w, http.StatusUnprocessableEntity, "verify failed", err.Error())
		return
	}
	s.Metrics.Verifies.Add(1)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handlePropose(w http.ResponseWriter, r *http.Request) {
	var req memory.ProposalRequest
	if !decode(w, r, &req) {
		return
	}
	p := principalFrom(r)
	resp, err := s.Svc.Cards.Propose(r.Context(), p, req)
	if err != nil {
		problem(w, http.StatusUnprocessableEntity, "proposal rejected", err.Error())
		return
	}
	s.Metrics.Proposals.Add(1)
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleListCards(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	cardsList, err := s.Svc.Store.ListCards(r.Context(), p.TenantID, listFilter(q.Get("namespace"), q.Get("status"), q.Get("card_type"), limit))
	if err != nil {
		problem(w, http.StatusInternalServerError, "list failed", err.Error())
		return
	}
	// ACL filter: no existence oracle for cards the principal can't read.
	visible := make([]memory.Card, 0, len(cardsList))
	for _, c := range cardsList {
		if authz.Allowed(p, c.ACL) {
			visible = append(visible, c)
		} else {
			s.Metrics.ACLDenials.Add(1)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"cards": visible})
}

func (s *Server) handleGetCard(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	card, err := s.Svc.Store.GetCard(r.Context(), p.TenantID, chi.URLParam(r, "id"))
	if err != nil || !authz.Allowed(p, card.ACL) {
		if err == nil {
			s.Metrics.ACLDenials.Add(1)
		}
		notFound(w)
		return
	}
	// mem.open returns citations + verification history (spec §7.1).
	var history []memory.VerificationResult
	if v, ok, err := s.Svc.Store.LatestVerification(r.Context(), card.ID); err == nil && ok {
		history = append(history, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"card": card, "verifications": history})
}

func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	var req memory.ReviewRequest
	if !decode(w, r, &req) {
		return
	}
	p := principalFrom(r)
	resp, err := s.Svc.Cards.Review(r.Context(), p, chi.URLParam(r, "id"), req)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			notFound(w)
			return
		}
		problem(w, http.StatusUnprocessableEntity, "review failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleInvalidate(w http.ResponseWriter, r *http.Request) {
	var req memory.InvalidateRequest
	if !decode(w, r, &req) {
		return
	}
	p := principalFrom(r)
	if err := s.Svc.Cards.Invalidate(r.Context(), p, chi.URLParam(r, "id"), req); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			notFound(w)
			return
		}
		problem(w, http.StatusUnprocessableEntity, "invalidate failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "recorded",
		"effect": "counter-evidence logged; card re-verification queued",
	})
}

// handleInsertFact lets connectors/benchmarks assert facts (facts are also
// produced by extraction; this is the direct write path with citations).
func (s *Server) handleInsertFact(w http.ResponseWriter, r *http.Request) {
	var f memory.Fact
	if !decode(w, r, &f) {
		return
	}
	p := principalFrom(r)
	if f.Subject == "" || f.Predicate == "" || f.Object == "" || f.NamespaceID == "" {
		problem(w, http.StatusUnprocessableEntity, "invalid fact", "subject, predicate, object, namespace_id required")
		return
	}
	if len(f.Sources) == 0 {
		problem(w, http.StatusUnprocessableEntity, "citation required", "facts need sources (I2/B4)")
		return
	}
	f.TenantID = p.TenantID
	f.CreatedBy = p.ID
	if len(f.ACL.Read) == 0 {
		f.ACL.Read = []string{"tenant:" + p.TenantID}
	}
	id, err := s.Svc.Store.InsertFact(r.Context(), f)
	if err != nil {
		problem(w, http.StatusUnprocessableEntity, "insert fact failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"fact_id": id, "status": orDefaultStr(f.Status, memory.FactProposed)})
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	var a memory.MemoryAction
	if !decode(w, r, &a) {
		return
	}
	p := principalFrom(r)
	a.TenantID = p.TenantID
	a.Principal = p.ID
	if a.NamespaceID == "" || a.ActionType == "" {
		problem(w, http.StatusUnprocessableEntity, "invalid action", "namespace_id and action_type required")
		return
	}
	if a.Input == nil {
		a.Input = map[string]any{}
	}
	if err := s.Svc.Store.InsertAction(r.Context(), a); err != nil {
		problem(w, http.StatusUnprocessableEntity, "record action failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleUpsertNamespace(w http.ResponseWriter, r *http.Request) {
	var ns memory.Namespace
	if !decode(w, r, &ns) {
		return
	}
	p := principalFrom(r)
	ns.TenantID = p.TenantID
	if ns.ID == "" || ns.Kind == "" {
		problem(w, http.StatusUnprocessableEntity, "invalid namespace", "id and kind required")
		return
	}
	if err := s.Svc.Store.UpsertNamespace(r.Context(), ns); err != nil {
		problem(w, http.StatusUnprocessableEntity, "namespace upsert failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ns)
}

// handleHealth serves the §16 memory-health dashboard data for the caller's
// tenant.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	h, err := s.Svc.Store.Health(r.Context(), p.TenantID)
	if err != nil {
		problem(w, http.StatusInternalServerError, "health failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":  p.TenantID,
		"health":  h,
		"process": map[string]any{
			"queries":       s.Metrics.Queries.Load(),
			"inserts":       s.Metrics.Inserts.Load(),
			"stale_served":  s.Metrics.StaleServed.Load(),
			"acl_denials":   s.Metrics.ACLDenials.Load(),
			"quarantines":   s.Metrics.Quarantines.Load(),
			"rate_limited":  s.Metrics.RateLimited.Load(),
		},
	})
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	var req BootstrapRequest
	if !decode(w, r, &req) {
		return
	}
	p := principalFrom(r)
	report, err := s.Svc.Bootstrap(r.Context(), p, req)
	if err != nil {
		problem(w, http.StatusUnprocessableEntity, "bootstrap failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// --- helpers -----------------------------------------------------------------

func listFilter(ns, status, cardType string, limit int) (f store.CardFilter) {
	f.NamespaceID, f.Status, f.CardType, f.Limit = ns, status, cardType, limit
	return
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		problem(w, http.StatusBadRequest, "bad request", "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// problem writes an RFC 7807 body (spec §13).
func problem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(memory.Problem{
		Type: "about:blank", Title: title, Status: status, Detail: detail,
	})
}

// notFound: 403 is never distinguishable from 404 — no existence oracle (§13).
func notFound(w http.ResponseWriter) {
	problem(w, http.StatusNotFound, "not found", "the object does not exist or you cannot see it")
}

func orDefaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

