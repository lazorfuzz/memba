// Package store defines the persistence interfaces of memd (spec §14.2).
// The only implementation today is store/postgres; every method takes an
// explicit tenant ID and every SQL statement in the implementation carries a
// tenant_id predicate — cross-tenant reads are structurally impossible
// (spec §4.3).
package store

import (
	"context"
	"time"

	"github.com/lazorfuzz/memba/pkg/memory"
)

// ScopeFilter restricts retrieval to a tenant, a namespace set (hints +
// ancestors, already resolved), and the caller's ACL subjects.
type ScopeFilter struct {
	TenantID    string
	Namespaces  []string
	ACLSubjects []string
}

// Candidate is one retrieval hit prior to fusion/rerank.
type Candidate struct {
	Kind        string // "chunk" | "card" | "fact"
	ID          string
	NamespaceID string
	RawID       string
	SourceURI   string
	SourceType  string
	Path        string
	LineStart   int
	LineEnd     int
	Title       string
	Body        string
	CardType    string
	Status      string
	Confidence  float64
	Structured  map[string]any
	Subject     string
	Predicate   string
	Object      string
	ValidFrom   *time.Time
	ValidTo     *time.Time
	EventTime   *time.Time
	TokenCount  int
	Score       float64 // retriever-local score (only used for ranking within a retriever)
	LastVerifiedAt *time.Time
	VerifyBy       *time.Time
	TTLClass       string
}

// ChunkRow is a chunk plus its embedding literal for insertion.
type ChunkRow struct {
	RawID      string
	Ordinal    int
	Kind       string
	Path       string
	LineStart  int
	LineEnd    int
	TokenCount int
	Body       string
	IdentText  string
	Symbols    []string
	// VectorLiteral is the pgvector text form "[…]"; empty = no embedding.
	VectorLiteral  string
	EmbeddingModel string
	Quarantined    bool
}

// Job is one background job row.
type Job struct {
	ID          int64
	Kind        string
	TenantID    string
	Payload     []byte
	Attempts    int
	MaxAttempts int
}

// CardPair is a near-duplicate or co-cited card pair (consolidation C1/C4).
type CardPair struct {
	A, B  memory.Card
	Score float64 // cosine similarity (C1) or shared-source count (C4)
}

// GapQuery is a clustered low-answerability query (consolidation C5).
type GapQuery struct {
	Query string
	Count int
}

// CardFilter selects cards for listing.
type CardFilter struct {
	NamespaceID string
	Status      string
	CardType    string
	Limit       int
}

// Store is the full persistence surface used by memd.
type Store interface {
	// Migrations / lifecycle
	Ping(ctx context.Context) error

	// Namespaces
	UpsertNamespace(ctx context.Context, ns memory.Namespace) error
	GetNamespace(ctx context.Context, tenantID, id string) (memory.Namespace, error)
	// ResolveScope expands namespace hints to include all ancestors (spec §4.3).
	ResolveScope(ctx context.Context, tenantID string, hints []string) ([]string, error)

	// Raw evidence
	InsertEvidence(ctx context.Context, tenantID string, ev memory.RawEvidence) (memory.RawEvidence, bool, error)
	GetEvidence(ctx context.Context, tenantID, id string) (memory.RawEvidence, error)

	// Chunks
	InsertChunks(ctx context.Context, tenantID, namespaceID string, acl memory.ACL, rows []ChunkRow) error
	SearchChunksFTS(ctx context.Context, f ScopeFilter, query string, n int) ([]Candidate, error)
	SearchChunksTrgm(ctx context.Context, f ScopeFilter, identQuery string, n int) ([]Candidate, error)
	SearchChunksVector(ctx context.Context, f ScopeFilter, vectorLiteral string, n int) ([]Candidate, error)
	SearchChunksSymbols(ctx context.Context, f ScopeFilter, symbols []string, n int) ([]Candidate, error)

	// Cards
	InsertCard(ctx context.Context, card memory.Card, vectorLiteral string) (string, error)
	GetCard(ctx context.Context, tenantID, id string) (memory.Card, error)
	ListCards(ctx context.Context, tenantID string, filter CardFilter) ([]memory.Card, error)
	UpdateCardStatus(ctx context.Context, tenantID, id, status, reason string) error
	SetCardVerified(ctx context.Context, tenantID, id string, verifiedAt, verifyBy time.Time, confidence float64) error
	PromoteCard(ctx context.Context, tenantID, id string, confidence float64) error
	TouchCardsRetrieved(ctx context.Context, tenantID string, ids []string) error
	SearchCardsFTS(ctx context.Context, f ScopeFilter, query string, statuses []string, n int) ([]Candidate, error)
	SearchCardsVector(ctx context.Context, f ScopeFilter, vectorLiteral string, statuses []string, n int) ([]Candidate, error)
	// NearestActiveCard returns the closest non-dormant card with the same
	// subject for dedup-at-propose (spec §10.8); ok=false when none.
	NearestActiveCard(ctx context.Context, tenantID, namespaceID, subject, vectorLiteral string) (memory.Card, float64, bool, error)
	CardSources(ctx context.Context, cardID string) ([]memory.SourceRef, error)
	AddCardSource(ctx context.Context, cardID string, ref memory.SourceRef) error
	InsertCardLink(ctx context.Context, srcID, dstID, linkType, createdBy string, weight float64) error
	ExpandCardLinks(ctx context.Context, f ScopeFilter, seedIDs []string, maxDepth, fanoutCap int) ([]Candidate, error)
	InsertReview(ctx context.Context, cardID, reviewer, decision, reason, editedBody string) error
	LatestApproval(ctx context.Context, cardID string) (bool, error)
	// Sweeps
	CardsVerifyDue(ctx context.Context, tenantID string, before time.Time, limit int) ([]memory.Card, error)
	CardsDecayDue(ctx context.Context, tenantID string, dormantMultiple int, limit int) ([]memory.Card, error)

	// Facts
	InsertFact(ctx context.Context, fact memory.Fact) (string, error)
	SearchFacts(ctx context.Context, f ScopeFilter, terms []string, asOf *time.Time, n int) ([]Candidate, error)
	ActiveFactConflicts(ctx context.Context, f ScopeFilter) ([][]memory.Fact, error)
	FactByID(ctx context.Context, tenantID, id string) (memory.Fact, error)
	SupersedeFact(ctx context.Context, tenantID, oldID, newID string) error
	RetractFact(ctx context.Context, tenantID, id string) error
	FactSources(ctx context.Context, factID string) ([]memory.SourceRef, error)
	AddFactSource(ctx context.Context, factID string, ref memory.SourceRef) error

	// Verifications
	InsertVerification(ctx context.Context, tenantID, namespaceID string, v memory.VerificationResult) (string, error)
	CachedVerification(ctx context.Context, targetID, repo, headSHA string) (memory.VerificationResult, bool, error)
	LatestVerification(ctx context.Context, targetID string) (memory.VerificationResult, bool, error)

	// Actions
	InsertAction(ctx context.Context, a memory.MemoryAction) error

	// Workspaces
	InsertWorkspace(ctx context.Context, tenantID, namespaceID, principal, query, mode, objectKey string, manifest memory.WorkspaceManifest, tokenTotal int, expiresAt time.Time) (string, error)
	GetWorkspace(ctx context.Context, tenantID, id string) (memory.WorkspaceManifest, string, error)
	ExpiredWorkspaces(ctx context.Context, now time.Time, limit int) ([]struct{ ID, TenantID, ObjectKey string }, error)
	DeleteWorkspace(ctx context.Context, tenantID, id string) error

	// Jobs
	EnqueueJob(ctx context.Context, tenantID, kind string, payload any) error
	DequeueJob(ctx context.Context, kinds []string) (*Job, error)
	CompleteJob(ctx context.Context, id int64) error
	FailJob(ctx context.Context, id int64, errMsg string) error

	// Discovery (cron scheduling)
	ListTenants(ctx context.Context) ([]string, error)
	ListNamespaces(ctx context.Context, tenantID string) ([]memory.Namespace, error)

	// Extraction support (§10.4 caps)
	CountCardsCreatedSince(ctx context.Context, tenantID, namespaceID, createdFrom string, since time.Time) (int, error)

	// Consolidation support (§11)
	NearDuplicateCardPairs(ctx context.Context, tenantID, namespaceID string, minCosine float64, limit int) ([]CardPair, error)
	CardHasSuperseder(ctx context.Context, cardID string) (bool, error)
	CoCitedCardPairs(ctx context.Context, tenantID, namespaceID string, minShared, limit int) ([]CardPair, error)
	LowAnswerabilityQueries(ctx context.Context, tenantID, namespaceID string, since time.Time, limit int) ([]GapQuery, error)
	FailedRunEvidence(ctx context.Context, tenantID, namespaceID string, since time.Time, limit int) ([]memory.RawEvidence, error)
	InsertConsolidationRun(ctx context.Context, tenantID, namespaceID string, stats map[string]any, runErr string) error
}
