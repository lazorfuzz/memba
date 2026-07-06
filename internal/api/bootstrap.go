package api

// Server-side cold start (spec §20, POST /v1/admin/bootstrap): ingests
// docs/build/CI files from the verifier's clone of the repo, seeds
// procedure cards from Makefile/justfile targets, and branch-verifies them
// immediately so code-backed seeds promote via P3 with no human input.

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/lazorfuzz/memba/internal/bootstrap"
	"github.com/lazorfuzz/memba/pkg/memory"
)

// BootstrapRequest is the body of POST /v1/admin/bootstrap.
type BootstrapRequest struct {
	Repo        string `json:"repo"`         // clone name under verify.repo_root
	NamespaceID string `json:"namespace_id"` // target namespace (created if absent)
}

// BootstrapReport mirrors the §20 step-4 report.
type BootstrapReport struct {
	FilesIngested  int      `json:"files_ingested"`
	CardsProposed  int      `json:"cards_proposed"`
	CardsPromoted  int      `json:"cards_promoted"`
	Gaps           []string `json:"gaps"`
	ReviewQueueURL string   `json:"review_queue"`
}

// Bootstrap runs §20 steps 1–4 over the local clone at
// verify.repo_root/<repo> (maintained by the git connector).
func (s *Service) Bootstrap(ctx context.Context, p memory.Principal, req BootstrapRequest) (BootstrapReport, error) {
	var report BootstrapReport
	if req.Repo == "" || req.NamespaceID == "" {
		return report, fmt.Errorf("repo and namespace_id are required")
	}
	dir := filepath.Join(s.Cfg.Verify.RepoRoot, filepath.Base(filepath.Clean(req.Repo)))

	if err := s.Store.UpsertNamespace(ctx, memory.Namespace{
		ID: req.NamespaceID, TenantID: p.TenantID, Kind: "repo",
	}); err != nil {
		return report, err
	}

	// 1. Ingest docs/build/CI files verbatim (through the normal §10.1
	// pipeline: scans, chunking, indexing).
	files := bootstrap.CollectFiles(dir)
	if len(files) == 0 {
		return report, fmt.Errorf("no bootstrap files under %s — is the clone present? (git connector populates verify.repo_root)", dir)
	}
	head := s.headSHA(ctx, req.Repo)
	byRel := map[string]string{}
	for _, f := range files {
		byRel[f.Rel] = f.Body
		_, err := s.Ingest.Insert(ctx, p, memory.InsertRequest{
			NamespaceID:   req.NamespaceID,
			SourceType:    memory.SourceGit,
			SourceURI:     "git://" + req.Repo + "/" + f.Rel,
			SourceVersion: head,
			Body:          f.Body,
			Metadata:      map[string]any{"repo": req.Repo, "path": f.Rel, "bootstrap": true},
		})
		if err != nil {
			continue
		}
		report.FilesIngested++
	}

	// 2+3. Seed procedure cards from build targets; verify immediately (P3).
	for _, mk := range []string{"Makefile", "justfile"} {
		body, ok := byRel[mk]
		if !ok {
			continue
		}
		for _, target := range bootstrap.MakeTargets(body) {
			resp, err := s.Cards.Propose(ctx, p, memory.ProposalRequest{
				NamespaceID: req.NamespaceID,
				CardType:    memory.CardProcedure,
				Title:       fmt.Sprintf("%s: run `make %s`", req.Repo, target),
				Body:        fmt.Sprintf("The %s target %q is defined in %s. Run `make %s` from the repo root.", req.Repo, target, mk, target),
				Subject:     req.Repo,
				Structured: map[string]any{
					"steps":          []string{fmt.Sprintf("run `make %s`", target)},
					"verify_command": "make " + target,
				},
				SourceRefs: []memory.SourceRef{{
					SourceURI: "git://" + req.Repo + "/" + mk, Path: mk, Quote: target + ":",
				}},
				Metadata: map[string]any{"bootstrap": true},
			})
			if err != nil {
				continue
			}
			report.CardsProposed++
			if resp.Status == memory.StatusActive {
				report.CardsPromoted++
				continue
			}
			if vr, err := s.Verify(ctx, p, memory.VerifyRequest{
				Target: "card:" + resp.CardID, VerificationType: memory.VerifyCodeBranchCheck,
				Repo: req.Repo,
			}); err == nil && vr.Result == memory.VerifyPassed {
				report.CardsPromoted++
			}
		}
	}

	// 4. Gaps + trigger profile generation (C2) right away.
	if _, ok := byRel["CODEOWNERS"]; !ok {
		if _, ok := byRel[".github/CODEOWNERS"]; !ok {
			report.Gaps = append(report.Gaps, "no CODEOWNERS found — ownership cards need human input")
		}
	}
	if _, ok := byRel["Makefile"]; !ok {
		if _, ok := byRel["justfile"]; !ok {
			report.Gaps = append(report.Gaps, "no Makefile/justfile — build/test commands not seeded")
		}
	}
	_ = s.Store.EnqueueJob(ctx, p.TenantID, "consolidate_ns", map[string]any{"namespace_id": req.NamespaceID})
	report.ReviewQueueURL = "/v1/cards?namespace=" + req.NamespaceID + "&status=proposed"
	return report, nil
}

func (s *Service) headSHA(ctx context.Context, repo string) string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := s.Verifier.VerifyAgainstBranch(cctx, memory.Card{}, nil, repo, "")
	if err == nil {
		return res.HeadSHA
	}
	return ""
}
