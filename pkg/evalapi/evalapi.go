// Package evalapi defines the BenchmarkMemory interface (spec §17.1).
// Benchmarks exercise the same public /v1 API as production — no
// benchmark-only read or write paths (I5).
package evalapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/pkg/client"
	"github.com/lazorfuzz/memba/pkg/memory"
)

// BenchmarkItem is one insertable piece of history.
type BenchmarkItem struct {
	NamespaceID string         `json:"namespace_id"`
	SourceType  string         `json:"source_type"`
	SourceURI   string         `json:"source_uri"`
	Title       string         `json:"title,omitempty"`
	Body        string         `json:"body"`
	EventTime   *time.Time     `json:"event_time,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// BenchmarkQuery is one question posed to memory.
type BenchmarkQuery struct {
	NamespaceID string     `json:"namespace_id"`
	Query       string     `json:"query"`
	Mode        string     `json:"mode,omitempty"`
	Repo        string     `json:"repo,omitempty"`
	Branch      string     `json:"branch,omitempty"`
	AsOf        *time.Time `json:"as_of,omitempty"`
	// Expected answers/abstention for scoring (never sent to the API).
	Expected   []string `json:"expected,omitempty"`
	Abstain    bool     `json:"abstain,omitempty"`
	CaseID     string   `json:"case_id,omitempty"`
	CaseFamily string   `json:"case_family,omitempty"`
}

// BenchmarkMemory drives memba through its public API (spec §17.1).
type BenchmarkMemory interface {
	Reset(ctx context.Context, namespace string) error
	Insert(ctx context.Context, item BenchmarkItem) error
	Query(ctx context.Context, q BenchmarkQuery) (memory.EvidencePack, error)
	RecordAction(ctx context.Context, a memory.MemoryAction) error
}

// V1 implements BenchmarkMemory over pkg/client.
type V1 struct {
	Client *client.Client
	// Namespaces are created under this tenant-scoped run prefix so Reset
	// guarantees isolation (§17.6) without deleting rows: each reset mints a
	// fresh child namespace.
	runSuffix string
}

func NewV1(c *client.Client) *V1 { return &V1{Client: c} }

// Reset registers a fresh benchmark namespace. Raw evidence is immutable
// (I1), so isolation comes from namespacing, not deletion.
func (v *V1) Reset(ctx context.Context, namespace string) error {
	v.runSuffix = fmt.Sprintf("/run-%d", time.Now().UnixNano())
	return v.Client.UpsertNamespace(ctx, memory.Namespace{
		ID: namespace + v.runSuffix, Kind: "repo",
	})
}

// ActiveNamespace returns the current isolated namespace for a base name.
func (v *V1) ActiveNamespace(base string) string { return base + v.runSuffix }

func (v *V1) Insert(ctx context.Context, item BenchmarkItem) (err error) {
	meta := item.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	meta["benchmark_seed"] = true // excluded from consolidation/training (§17.6)
	_, err = v.Client.InsertEvidence(ctx, memory.InsertRequest{
		NamespaceID: v.ActiveNamespace(item.NamespaceID),
		SourceType:  item.SourceType,
		SourceURI:   item.SourceURI,
		// The run id doubles as source_version: identical corpora re-run
		// under a new Reset() get fresh rows in the fresh namespace instead
		// of deduping to a prior run's rows (idempotency key includes
		// version; §13.2).
		SourceVersion: strings.TrimPrefix(v.runSuffix, "/"),
		Title:         item.Title,
		Body:          item.Body,
		EventTime:     item.EventTime,
		Metadata:      meta,
	})
	return err
}

func (v *V1) Query(ctx context.Context, q BenchmarkQuery) (memory.EvidencePack, error) {
	mode := q.Mode
	if mode == "" {
		mode = memory.ModeDeep
	}
	return v.Client.Query(ctx, memory.QueryRequest{
		NamespaceHints: []string{v.ActiveNamespace(q.NamespaceID)},
		Query:          q.Query,
		Mode:           mode,
		Repo:           q.Repo,
		Branch:         q.Branch,
		AsOf:           q.AsOf,
	})
}

func (v *V1) RecordAction(ctx context.Context, a memory.MemoryAction) error {
	return v.Client.RecordAction(ctx, a)
}
