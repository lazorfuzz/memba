// memd is the single memba service binary (spec §4.1): API, ingest,
// retrieval, verification, background jobs, workspace export.
//
//	memd --config configs/memd.yaml --role all|api|worker [--migrate]
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lazorfuzz/memba/internal/api"
	"github.com/lazorfuzz/memba/internal/config"
	"github.com/lazorfuzz/memba/internal/consolidate"
	"github.com/lazorfuzz/memba/internal/embed"
	"github.com/lazorfuzz/memba/internal/extract"
	"github.com/lazorfuzz/memba/internal/jobs"
	"github.com/lazorfuzz/memba/internal/rerank"
	"github.com/lazorfuzz/memba/internal/store/postgres"
	"github.com/lazorfuzz/memba/internal/verify"
	"github.com/lazorfuzz/memba/internal/workspace"

	objstorepkg "github.com/lazorfuzz/memba/internal/objstore"
)

func main() {
	var (
		configPath = flag.String("config", "configs/memd.yaml", "config file")
		role       = flag.String("role", "all", "api|worker|all")
		migrate    = flag.Bool("migrate", false, "apply migrations and exit")
		sweepEvery = flag.Duration("sweep-every", time.Hour, "sweep scheduling cadence")
		tenants    = flag.String("tenants", "", "comma-separated tenants for cron sweeps")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	dsn := os.Getenv(cfg.Postgres.DSNEnv)
	if dsn == "" {
		log.Error("missing DSN", "env", cfg.Postgres.DSNEnv)
		os.Exit(1)
	}
	if *migrate {
		if err := postgres.Migrate(dsn); err != nil {
			log.Error("migrate", "err", err)
			os.Exit(1)
		}
		log.Info("migrations applied")
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := postgres.New(ctx, dsn, cfg.Postgres.MaxConns)
	if err != nil {
		log.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	emb, err := embed.New(cfg.Models.Embedder)
	if err != nil {
		log.Error("embedder", "err", err)
		os.Exit(1)
	}
	blobs, err := objstorepkg.New(cfg.ObjStore)
	if err != nil {
		log.Error("objstore", "err", err)
		os.Exit(1)
	}
	ws := &workspace.Writer{
		Store: st, Blobs: blobs, TTLHours: cfg.Security.WorkspaceTTLHours,
		ModelIDs: map[string]string{
			"embedder": emb.ModelID(),
			"reranker": rerank.LexicalReranker{}.ModelID(),
		},
	}
	svc := api.New(cfg, st, emb, rerank.LexicalReranker{}, ws)
	log.Info("memba", "config_hash", svc.Hash, "embedder", emb.ModelID())

	if *role == "worker" || *role == "all" {
		verifier := &verify.Verifier{Store: st, Embedder: emb, RepoRoot: cfg.Verify.RepoRoot}

		// LLM extractor (§10.4): enabled only when the configured provider has
		// credentials; otherwise extract_cards jobs drain as recorded no-ops.
		llm, err := extract.NewLLM(cfg.Models.Extractor)
		if err != nil {
			log.Error("extractor", "err", err)
			os.Exit(1)
		}
		var extractor *extract.Extractor
		if llm != nil {
			system, user := extract.LoadPrompts("configs/prompts")
			extractor = &extract.Extractor{
				Store: st, Cards: svc.Cards, LLM: llm,
				SystemPrompt: system, UserPrompt: user,
				MaxCandidates: 8, DailyCap: 200,
			}
			log.Info("extractor enabled", "model", llm.ModelID())
		} else {
			log.Info("extractor disabled (no provider credentials); extract_cards jobs will no-op")
		}

		w := &jobs.Worker{
			Store: st, Cards: svc.Cards,
			Verifier:  verifier,
			Workspace: ws,
			Extractor: extractor,
			Consolidator: &consolidate.Consolidator{
				Store: st, Cards: svc.Cards, Blobs: blobs, Cfg: cfg, Checker: verifier,
			},
			Log: log, SweepEvery: *sweepEvery,
			Tenants: splitNonEmpty(*tenants),
		}
		go w.Run(ctx)
	}

	if *role == "api" || *role == "all" {
		srv, err := api.NewServer(svc)
		if err != nil {
			log.Error("server", "err", err)
			os.Exit(1)
		}
		httpSrv := &http.Server{
			Addr:              cfg.Server.HTTPAddr,
			Handler:           srv.Router(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(shutdownCtx)
		}()
		log.Info("listening", "addr", cfg.Server.HTTPAddr, "role", *role)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http", "err", err)
			os.Exit(1)
		}
		return
	}
	<-ctx.Done()
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
