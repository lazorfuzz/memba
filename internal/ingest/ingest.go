// Package ingest implements the write path of spec §10.1: normalize →
// store raw verbatim (I1) → security scans (§15.3) → chunk → embed → index.
//
// Chunking and embedding run inline (the local embedder is cheap and this
// keeps Insert→Query read-your-writes for benchmarks, I5); the extract_cards
// stage is enqueued as a background job for an LLM extractor worker.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/lazorfuzz/memba/internal/chunk"
	"github.com/lazorfuzz/memba/internal/config"
	"github.com/lazorfuzz/memba/internal/embed"
	"github.com/lazorfuzz/memba/internal/secscan"
	"github.com/lazorfuzz/memba/internal/store"
	"github.com/lazorfuzz/memba/pkg/memory"
)

type Service struct {
	Store    store.Store
	Embedder embed.Embedder
	Cfg      config.Config
}

const inlineBodyLimit = 256 * 1024 // §6.2: larger bodies belong in the object store

// Insert runs the §10.1 pipeline for one evidence item.
func (s *Service) Insert(ctx context.Context, p memory.Principal, req memory.InsertRequest) (memory.InsertResponse, error) {
	if req.NamespaceID == "" || req.SourceType == "" || req.SourceURI == "" || req.Body == "" {
		return memory.InsertResponse{}, fmt.Errorf("namespace_id, source_type, source_uri, body are required")
	}
	if !validSourceType(req.SourceType) {
		return memory.InsertResponse{}, fmt.Errorf("unknown source_type %q", req.SourceType)
	}
	if len(req.ACL.Read) == 0 {
		// Default visibility: the ingesting tenant.
		req.ACL.Read = []string{"tenant:" + p.TenantID}
	}

	sum := sha256.Sum256([]byte(req.Body))
	ev := memory.RawEvidence{
		NamespaceID:      req.NamespaceID,
		SourceType:       req.SourceType,
		SourceURI:        req.SourceURI,
		SourceExternalID: req.SourceExternalID,
		SourceVersion:    req.SourceVersion,
		ContentHash:      hex.EncodeToString(sum[:]),
		Title:            req.Title,
		Body:             req.Body,
		Metadata:         req.Metadata,
		ACL:              req.ACL,
		EventTime:        req.EventTime,
	}

	// Security scans (§15.3, §15.4 D2) run on the verbatim body BEFORE
	// storage decisions. Raw stays verbatim (I1); chunks get redactions.
	secrets := secscan.ScanSecrets(req.Body, s.Cfg.Security.SecretScan.EntropyThreshold)
	kind := chunk.KindFor(req.SourceType, pathOf(req))
	var injections []secscan.InjectionFinding
	if s.Cfg.Security.InjectionScan.Enabled {
		injections = secscan.ScanInjection(req.Body, kind)
	}
	if len(secrets) > 0 {
		ev.Quarantined = true
		ev.EmbeddingForbidden = true // secret-bearing: never embed (§6.2)
	}
	if len(injections) > 0 {
		ev.Quarantined = true
	}
	if ev.Quarantined {
		ev.QuarantineReason = secscan.Summary(secrets, injections)
	}

	stored, deduped, err := s.Store.InsertEvidence(ctx, p.TenantID, ev)
	if err != nil {
		return memory.InsertResponse{}, err
	}
	resp := memory.InsertResponse{
		RawID:         stored.ID,
		Deduplicated:  deduped,
		Quarantined:   stored.Quarantined,
		QuarantineWhy: stored.QuarantineReason,
	}
	// Dedup re-posts still run the (conflict-free) index pipeline below:
	// chunk inserts are ON CONFLICT DO NOTHING, so this heals evidence whose
	// first indexing attempt failed mid-way without duplicating anything.

	// Injection-suspect content is quarantined and never indexed for default
	// recall (D2). Secret-bearing content is chunked with redactions
	// (retrievable, harmless) but never embedded.
	if len(injections) > 0 {
		return resp, nil
	}

	body := req.Body
	var redactionsApplied bool
	if len(secrets) > 0 {
		body, _ = secscan.Redact(body, secrets)
		redactionsApplied = true
	}

	pieces := chunk.Split(kind, pathOf(req), body)
	rows := make([]store.ChunkRow, 0, len(pieces))
	texts := make([]string, 0, len(pieces))
	for _, pc := range pieces {
		rows = append(rows, store.ChunkRow{
			RawID: stored.ID, Ordinal: pc.Ordinal, Kind: pc.Kind, Path: pc.Path,
			LineStart: pc.LineStart, LineEnd: pc.LineEnd, TokenCount: pc.TokenCount,
			Body: pc.Body, IdentText: pc.IdentText, Symbols: pc.Symbols,
		})
		texts = append(texts, pc.Body)
	}
	if s.Embedder != nil && !ev.EmbeddingForbidden && !redactionsApplied && len(texts) > 0 {
		if vecs, err := s.Embedder.Embed(ctx, texts); err == nil {
			for i := range rows {
				if i < len(vecs) && vecs[i] != nil {
					rows[i].VectorLiteral = embed.VectorLiteral(vecs[i])
					rows[i].EmbeddingModel = s.Embedder.ModelID()
				}
			}
		}
	} else if redactionsApplied && s.Embedder != nil && len(texts) > 0 {
		// Redacted bodies are safe to embed (secrets replaced by markers).
		if vecs, err := s.Embedder.Embed(ctx, texts); err == nil {
			for i := range rows {
				if i < len(vecs) && vecs[i] != nil {
					rows[i].VectorLiteral = embed.VectorLiteral(vecs[i])
					rows[i].EmbeddingModel = s.Embedder.ModelID()
				}
			}
		}
	}
	if err := s.Store.InsertChunks(ctx, p.TenantID, req.NamespaceID, req.ACL, rows); err != nil {
		return resp, fmt.Errorf("index chunks: %w", err)
	}
	resp.ChunksEnqueued = true

	// Card/fact extraction is an offline LLM job (§10.4); once per new item.
	if !deduped {
		_ = s.Store.EnqueueJob(ctx, p.TenantID, "extract_cards", map[string]any{
			"raw_id": stored.ID, "namespace_id": req.NamespaceID,
		})
	}
	return resp, nil
}

func pathOf(req memory.InsertRequest) string {
	if p, ok := req.Metadata["path"].(string); ok && p != "" {
		return p
	}
	// git://repo/path/to/file.go → path/to/file.go
	if strings.HasPrefix(req.SourceURI, "git://") {
		rest := strings.TrimPrefix(req.SourceURI, "git://")
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			return rest[i+1:]
		}
	}
	return ""
}

func validSourceType(t string) bool {
	switch t {
	case memory.SourceGit, memory.SourceGithubPR, memory.SourceDoc, memory.SourceSlack,
		memory.SourceCI, memory.SourceIncident, memory.SourceTicket, memory.SourceAgentRun,
		memory.SourceManual:
		return true
	}
	return false
}
