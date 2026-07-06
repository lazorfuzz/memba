// Package memory defines the public request/response types of the memba /v1
// API: evidence, chunks, cards, facts, verifications, evidence packs, and
// workspaces. These types are the stability contract between memd, the Go
// client, connectors, and the benchmark harness (spec §14.2, I5).
package memory

import "time"

// ---------------------------------------------------------------------------
// Principals and ACLs (spec §15.2)
// ---------------------------------------------------------------------------

// ACL is the read/write access control list stored on every object.
// Subjects look like "user:jdoe", "team:payments", "agent:coder-17",
// "svc:ingest", "tenant:acme".
type ACL struct {
	Read  []string `json:"read"`
	Write []string `json:"write,omitempty"`
}

// Principal is the authenticated caller.
type Principal struct {
	TenantID    string   `json:"tenant_id"`
	ID          string   `json:"principal"`    // e.g. "agent:payments-coder-17"
	ACLSubjects []string `json:"acl_subjects"` // subjects this principal holds
}

// ---------------------------------------------------------------------------
// Raw evidence (spec §6.2)
// ---------------------------------------------------------------------------

// SourceType enumerates raw evidence origins.
const (
	SourceGit      = "git"
	SourceGithubPR = "github_pr"
	SourceDoc      = "doc"
	SourceSlack    = "slack"
	SourceCI       = "ci"
	SourceIncident = "incident"
	SourceTicket   = "ticket"
	SourceAgentRun = "agent_run"
	SourceManual   = "manual"
)

// SourceAuthority ranks source types per spec §8.4 (1 = strongest).
func SourceAuthority(sourceType string) int {
	switch sourceType {
	case SourceGit:
		return 1
	case SourceCI:
		return 2
	case SourceDoc:
		return 3
	case SourceGithubPR:
		return 4
	case SourceIncident:
		return 5
	case SourceTicket:
		return 6
	case SourceSlack:
		return 7
	case SourceAgentRun:
		return 8
	case SourceManual:
		return 3 // curated human input, treated like maintained docs
	default:
		return 8
	}
}

// InsertRequest is the body of POST /v1/evidence (spec §13.2).
type InsertRequest struct {
	NamespaceID      string         `json:"namespace_id"`
	SourceType       string         `json:"source_type"`
	SourceURI        string         `json:"source_uri"`
	SourceExternalID string         `json:"source_external_id,omitempty"`
	SourceVersion    string         `json:"source_version,omitempty"`
	Title            string         `json:"title,omitempty"`
	Body             string         `json:"body"`
	ACL              ACL            `json:"acl"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	EventTime        *time.Time     `json:"event_time,omitempty"`
}

// InsertResponse is returned by POST /v1/evidence.
type InsertResponse struct {
	RawID          string `json:"raw_id"`
	Deduplicated   bool   `json:"deduplicated"`   // idempotent re-post
	ChunksEnqueued bool   `json:"chunks_enqueued"`
	Quarantined    bool   `json:"quarantined"`
	QuarantineWhy  string `json:"quarantine_reason,omitempty"`
}

// RawEvidence is a stored verbatim artifact (immutable, I1).
type RawEvidence struct {
	ID                 string         `json:"id"`
	TenantID           string         `json:"tenant_id"`
	NamespaceID        string         `json:"namespace_id"`
	SourceType         string         `json:"source_type"`
	SourceURI          string         `json:"source_uri"`
	SourceExternalID   string         `json:"source_external_id,omitempty"`
	SourceVersion      string         `json:"source_version,omitempty"`
	ContentHash        string         `json:"content_hash"`
	Title              string         `json:"title,omitempty"`
	Body               string         `json:"body"`
	BodyObjectKey      string         `json:"body_object_key,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
	ACL                ACL            `json:"acl"`
	EventTime          *time.Time     `json:"event_time,omitempty"`
	IngestedAt         time.Time      `json:"ingested_at"`
	Quarantined        bool           `json:"quarantined"`
	QuarantineReason   string         `json:"quarantine_reason,omitempty"`
	EmbeddingForbidden bool           `json:"embedding_forbidden"`
}

// ---------------------------------------------------------------------------
// Chunks (spec §6.3)
// ---------------------------------------------------------------------------

const (
	ChunkCode   = "code"
	ChunkProse  = "prose"
	ChunkChat   = "chat"
	ChunkDiff   = "diff"
	ChunkLog    = "log"
	ChunkConfig = "config"
)

// Chunk is an indexed slice of raw evidence.
type Chunk struct {
	ID          string   `json:"id"`
	TenantID    string   `json:"tenant_id"`
	NamespaceID string   `json:"namespace_id"`
	RawID       string   `json:"raw_id"`
	Ordinal     int      `json:"ordinal"`
	Kind        string   `json:"kind"`
	Path        string   `json:"path,omitempty"`
	LineStart   int      `json:"line_start,omitempty"`
	LineEnd     int      `json:"line_end,omitempty"`
	TokenCount  int      `json:"token_count"`
	Body        string   `json:"body"`
	IdentText   string   `json:"ident_text,omitempty"`
	Symbols     []string `json:"symbols,omitempty"`
	ACL         ACL      `json:"acl"`
	Quarantined bool     `json:"quarantined"`
}

// ---------------------------------------------------------------------------
// Memory cards (spec §6.4)
// ---------------------------------------------------------------------------

// Card types.
const (
	CardFactNote        = "fact_note"
	CardConvention      = "convention"
	CardWarning         = "warning"
	CardGotcha          = "gotcha"
	CardProcedure       = "procedure"
	CardSkill           = "skill"
	CardDesignDecision  = "design_decision"
	CardOwnership       = "ownership"
	CardEnvironmentNote = "environment_note"
	CardDebuggingHint   = "debugging_hint"
)

// CardTypes lists all valid card types.
var CardTypes = []string{
	CardFactNote, CardConvention, CardWarning, CardGotcha, CardProcedure,
	CardSkill, CardDesignDecision, CardOwnership, CardEnvironmentNote,
	CardDebuggingHint,
}

// Card statuses (spec §6.4 status semantics).
const (
	StatusProposed    = "proposed"
	StatusActive      = "active"
	StatusStale       = "stale"
	StatusDeprecated  = "deprecated"
	StatusInvalidated = "invalidated"
	StatusQuarantined = "quarantined"
	StatusDormant     = "dormant"
)

// TTL classes (spec §10.7 / I6).
const (
	TTLCode        = "code"        // 28d default
	TTLOperational = "operational" // 90d default
	TTLDesign      = "design"      // 180d default
)

// Provenance of card creation.
const (
	FromHuman         = "human"
	FromExtractor     = "extractor"
	FromAgentRun      = "agent_run"
	FromConsolidation = "consolidation"
	FromBootstrap     = "bootstrap"
	FromBenchmarkSeed = "benchmark_seed"
)

// SourceRef is a citation from a card/fact to raw evidence (spec §6.5).
type SourceRef struct {
	RawID         string `json:"raw_id,omitempty"`
	ChunkID       string `json:"chunk_id,omitempty"`
	SourceURI     string `json:"source_uri"`
	SourceVersion string `json:"source_version,omitempty"`
	Path          string `json:"path,omitempty"`
	LineStart     int    `json:"line_start,omitempty"`
	LineEnd       int    `json:"line_end,omitempty"`
	Quote         string `json:"quote,omitempty"`
	SupportType   string `json:"support_type,omitempty"` // supports|contradicts|supersedes|validates
}

// Card is the main derived memory unit.
type Card struct {
	ID              string         `json:"id"`
	TenantID        string         `json:"tenant_id"`
	NamespaceID     string         `json:"namespace_id"`
	CardType        string         `json:"card_type"`
	Title           string         `json:"title"`
	Body            string         `json:"body"`
	Structured      map[string]any `json:"structured,omitempty"`
	Subject         string         `json:"subject,omitempty"`
	Tags            []string       `json:"tags,omitempty"`
	Entities        []string       `json:"entities,omitempty"`
	Status          string         `json:"status"`
	Confidence      float64        `json:"confidence"`
	Importance      float64        `json:"importance"`
	ValidFrom       *time.Time     `json:"valid_from,omitempty"`
	ValidTo         *time.Time     `json:"valid_to,omitempty"`
	TTLClass        string         `json:"ttl_class"`
	LastVerifiedAt  *time.Time     `json:"last_verified_at,omitempty"`
	VerifyBy        *time.Time     `json:"verify_by,omitempty"`
	LastRetrievedAt *time.Time     `json:"last_retrieved_at,omitempty"`
	CreatedFrom     string         `json:"created_from"`
	CreatedBy       string         `json:"created_by"`
	ACL             ACL            `json:"acl"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	Sources         []SourceRef    `json:"sources,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// ProposalRequest is the body of POST /v1/cards (mem.propose, spec §7.1).
type ProposalRequest struct {
	RunID       string         `json:"run_id,omitempty"`
	NamespaceID string         `json:"namespace_id"`
	CardType    string         `json:"card_type"`
	Title       string         `json:"title"`
	Body        string         `json:"body"`
	Structured  map[string]any `json:"structured,omitempty"`
	Subject     string         `json:"subject,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	TTLClass    string         `json:"ttl_class,omitempty"`
	SourceRefs  []SourceRef    `json:"source_refs"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// ProposalResponse is returned by POST /v1/cards.
type ProposalResponse struct {
	CardID        string `json:"card_id"`
	Status        string `json:"status"`
	PromotionHint string `json:"promotion_hint"`
	DuplicateOf   string `json:"near_duplicate_of,omitempty"`
}

// ReviewRequest is the body of POST /v1/cards/{id}/review.
type ReviewRequest struct {
	Decision   string `json:"decision"` // approve|reject|invalidate|edit
	Reason     string `json:"reason,omitempty"`
	EditedBody string `json:"edited_body,omitempty"`
}

// ReviewResponse is returned by POST /v1/cards/{id}/review.
type ReviewResponse struct {
	CardID string `json:"card_id"`
	Status string `json:"status"`
}

// InvalidateRequest is the body of POST /v1/cards/{id}/invalidate (mem.invalidate).
type InvalidateRequest struct {
	Reason       string   `json:"reason"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

// ---------------------------------------------------------------------------
// Facts (spec §6.6)
// ---------------------------------------------------------------------------

// Fact statuses.
const (
	FactProposed   = "proposed"
	FactActive     = "active"
	FactSuperseded = "superseded"
	FactRetracted  = "retracted"
)

// Fact is a bitemporal (subject, predicate, object) triple.
type Fact struct {
	ID          string      `json:"id"`
	TenantID    string      `json:"tenant_id"`
	NamespaceID string      `json:"namespace_id"`
	Subject     string      `json:"subject"`
	Predicate   string      `json:"predicate"`
	Object      string      `json:"object"`
	ObjectType  string      `json:"object_type"`
	ValidFrom   time.Time   `json:"valid_from"`
	ValidTo     *time.Time  `json:"valid_to,omitempty"`
	AssertedAt  time.Time   `json:"asserted_at"`
	RetractedAt *time.Time  `json:"retracted_at,omitempty"`
	Supersedes  string      `json:"supersedes,omitempty"`
	CardID      string      `json:"card_id,omitempty"`
	Status      string      `json:"status"`
	ACL         ACL         `json:"acl"`
	CreatedBy   string      `json:"created_by"`
	Verified    bool        `json:"verified"`
	Sources     []SourceRef `json:"sources,omitempty"`
}

// ---------------------------------------------------------------------------
// Verification (spec §9)
// ---------------------------------------------------------------------------

const (
	VerifyCodeBranchCheck    = "code_branch_check"
	VerifyTestPassed         = "test_passed"
	VerifyCommandSucceeded   = "command_succeeded"
	VerifySourceStillExists  = "source_still_exists"
	VerifyHumanReview        = "human_review"
	VerifyMultiSourceSupport = "multi_source_support"
)

const (
	VerifyPassed       = "passed"
	VerifyFailed       = "failed"
	VerifyInconclusive = "inconclusive"
)

// VerifyRequest is the body of POST /v1/verify (mem.verify).
type VerifyRequest struct {
	Target           string `json:"target"` // "card:<id>" | "fact:<id>"
	VerificationType string `json:"verification_type"`
	Repo             string `json:"repo,omitempty"`
	Branch           string `json:"branch,omitempty"`
}

// RefOutcome records the per-citation result of a verification check.
type RefOutcome struct {
	SourceURI string `json:"source_uri"`
	Check     string `json:"check"`  // file_exists|symbol_exists|quote_holds|commit_ancestor|source_exists
	Result    string `json:"result"` // passed|failed|inconclusive
	Detail    string `json:"detail,omitempty"`
}

// VerificationResult is returned by POST /v1/verify and recorded per card.
type VerificationResult struct {
	VerificationID string       `json:"verification_id"`
	TargetType     string       `json:"target_type"`
	TargetID       string       `json:"target_id"`
	Type           string       `json:"verification_type"`
	Result         string       `json:"result"`
	Repo           string       `json:"repo,omitempty"`
	Branch         string       `json:"branch,omitempty"`
	HeadSHA        string       `json:"head_sha,omitempty"`
	PerRef         []RefOutcome `json:"per_ref,omitempty"`
	VerifiedAt     time.Time    `json:"verified_at"`
}

// ---------------------------------------------------------------------------
// Query / evidence pack (spec §8)
// ---------------------------------------------------------------------------

// Query modes (spec §8.1).
const (
	ModeBoot      = "boot"
	ModeScoped    = "scoped"
	ModeDeep      = "deep"
	ModeWorkspace = "workspace"
	ModeAudit     = "audit"
	ModeBenchmark = "benchmark"
)

// QueryRequest is the body of POST /v1/query (spec §13.2).
type QueryRequest struct {
	NamespaceHints            []string   `json:"namespace_hints"`
	Query                     string     `json:"query"`
	GoalType                  string     `json:"goal_type,omitempty"` // coding_change|question|incident|onboarding
	Repo                      string     `json:"repo,omitempty"`
	Branch                    string     `json:"branch,omitempty"`
	Mode                      string     `json:"mode"`
	MaxTokens                 int        `json:"max_tokens,omitempty"`
	RequireCitations          bool       `json:"require_citations,omitempty"`
	RequireBranchVerification bool       `json:"require_branch_verification,omitempty"`
	AsOf                      *time.Time `json:"as_of,omitempty"`
}

// PackCard is a card as it appears inside an evidence pack.
type PackCard struct {
	ID             string         `json:"id"`
	CardType       string         `json:"card_type"`
	Title          string         `json:"title"`
	Body           string         `json:"body"`
	Structured     map[string]any `json:"structured,omitempty"`
	Status         string         `json:"status"`
	Confidence     float64        `json:"confidence"`
	Verified       bool           `json:"verified"`
	LastVerifiedAt *time.Time     `json:"last_verified_at,omitempty"`
	Sources        []SourceRef    `json:"sources,omitempty"`
}

// PackFact is a fact as it appears inside an evidence pack.
type PackFact struct {
	ID        string      `json:"id,omitempty"`
	Subject   string      `json:"subject"`
	Predicate string      `json:"predicate"`
	Object    string      `json:"object"`
	ValidFrom time.Time   `json:"valid_from"`
	ValidTo   *time.Time  `json:"valid_to,omitempty"`
	Verified  bool        `json:"verified"`
	Sources   []SourceRef `json:"sources,omitempty"`
}

// ConflictGroup is a set of contradicting claims with the deterministic
// winner annotation (spec §8.7).
type ConflictGroup struct {
	Claims     []any  `json:"claims"`
	Winner     string `json:"winner,omitempty"` // id of the winning claim
	Rule       string `json:"rule"`             // which rule decided
	Unresolved bool   `json:"unresolved,omitempty"`
}

// StaleFlagged is an item that failed JIT verification (served flagged, never
// silently dropped — spec §9.2).
type StaleFlagged struct {
	Card         *PackCard           `json:"card,omitempty"`
	Fact         *PackFact           `json:"fact,omitempty"`
	Verification *VerificationResult `json:"verification,omitempty"`
	Detail       string              `json:"detail,omitempty"`
}

// RawSpan is a labeled raw-evidence excerpt in a pack.
type RawSpan struct {
	RawID     string     `json:"raw_id"`
	ChunkID   string     `json:"chunk_id,omitempty"`
	SourceURI string     `json:"source_uri"`
	Authority int        `json:"authority"`
	Path      string     `json:"path,omitempty"`
	Lines     string     `json:"lines,omitempty"`
	EventTime *time.Time `json:"event_time,omitempty"`
	Excerpt   string     `json:"excerpt"`
}

// ReviewRecord is one review decision in an audit report.
type ReviewRecord struct {
	Reviewer  string    `json:"reviewer"`
	Decision  string    `json:"decision"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// EvidenceSummary describes cited raw evidence in an audit report without
// reproducing its body.
type EvidenceSummary struct {
	RawID       string    `json:"raw_id"`
	SourceType  string    `json:"source_type"`
	SourceURI   string    `json:"source_uri"`
	IngestedAt  time.Time `json:"ingested_at"`
	Quarantined bool      `json:"quarantined"`
}

// AuditReport is the mode=audit payload (spec §8.1): full provenance +
// verification chain for one memory (§15.4 D7 forensics).
type AuditReport struct {
	Ref           string               `json:"ref"`
	Card          *Card                `json:"card,omitempty"`
	Fact          *Fact                `json:"fact,omitempty"`
	Verifications []VerificationResult `json:"verifications"`
	Reviews       []ReviewRecord       `json:"reviews"`
	CitedEvidence []EvidenceSummary    `json:"cited_evidence"`
}

// EvidencePack is the structured response of POST /v1/query (spec §8.8).
type EvidencePack struct {
	QueryID         string          `json:"query_id"`
	Mode            string          `json:"mode"`
	Answerability   string          `json:"answerability"` // high|partial|low
	ConfigHash      string          `json:"config_hash"`
	ProfileExcerpt  string          `json:"profile_excerpt,omitempty"`
	BootContext     string          `json:"boot_context,omitempty"` // L0, boot mode only
	Cards           []PackCard      `json:"cards"`
	Facts           []PackFact      `json:"facts"`
	Superseded      []PackFact      `json:"superseded"`
	Conflicts       []ConflictGroup `json:"conflicts"`
	StaleFlagged    []StaleFlagged  `json:"stale_flagged"`
	RawSpans        []RawSpan       `json:"raw_spans"`
	MissingEvidence []string        `json:"missing_evidence"`
	TokenTotal      int             `json:"token_total"`
	WorkspaceURI    string          `json:"workspace_uri,omitempty"`
	WorkspaceID     string          `json:"workspace_id,omitempty"`
	Degraded        []string        `json:"degraded,omitempty"` // retrievers that failed (§14.5)
	Audit           *AuditReport    `json:"audit,omitempty"`    // mode=audit only
}

// ---------------------------------------------------------------------------
// Memory actions (spec §6.9)
// ---------------------------------------------------------------------------

// Action types.
const (
	ActionSearch         = "search"
	ActionOpen           = "open"
	ActionWorkspaceMount = "workspace_mount"
	ActionLog            = "log"
	ActionPropose        = "propose"
	ActionVerify         = "verify"
	ActionInvalidate     = "invalidate"
	ActionCite           = "cite"
	ActionIgnore         = "ignore"
	ActionPromoteRequest = "promote_request"
)

// MemoryAction is one recorded memory operation (instrumentation, I5).
type MemoryAction struct {
	ID          string         `json:"id,omitempty"`
	RunID       string         `json:"run_id,omitempty"`
	TenantID    string         `json:"tenant_id,omitempty"`
	NamespaceID string         `json:"namespace_id"`
	Principal   string         `json:"principal,omitempty"`
	ActionType  string         `json:"action_type"`
	Input       map[string]any `json:"input"`
	Output      map[string]any `json:"output,omitempty"`
	Success     *bool          `json:"success,omitempty"`
	LatencyMs   int            `json:"latency_ms,omitempty"`
	CreatedAt   time.Time      `json:"created_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Workspaces (spec §12)
// ---------------------------------------------------------------------------

// ManifestFile is one entry in the workspace manifest.
type ManifestFile struct {
	Path       string   `json:"path"`
	Kind       string   `json:"kind"`
	TokenCount int      `json:"token_count"`
	ItemIDs    []string `json:"item_ids,omitempty"`
}

// ACLDecision records why an item was included or excluded (audit, §12.2).
type ACLDecision struct {
	Item      string `json:"item"`
	Decision  string `json:"decision"` // included|excluded_acl
	Principal string `json:"principal"`
}

// Redaction records a secret redaction inside a workspace file.
type Redaction struct {
	File   string `json:"file"`
	Marker string `json:"marker"`
}

// WorkspaceManifest is manifest.json (spec §12.2).
type WorkspaceManifest struct {
	WorkspaceID  string            `json:"workspace_id"`
	TenantID     string            `json:"tenant_id"`
	NamespaceID  string            `json:"namespace_id"`
	Query        string            `json:"query"`
	Repo         string            `json:"repo,omitempty"`
	Branch       string            `json:"branch,omitempty"`
	Mode         string            `json:"mode"`
	CreatedAt    time.Time         `json:"created_at"`
	ExpiresAt    time.Time         `json:"expires_at"`
	ConfigHash   string            `json:"config_hash"`
	ModelIDs     map[string]string `json:"model_ids"`
	TokenTotal   int               `json:"token_total"`
	Files        []ManifestFile    `json:"files"`
	ACLDecisions []ACLDecision     `json:"acl_decisions"`
	Redactions   []Redaction       `json:"redactions,omitempty"`
}

// WorkspaceRef points at an exported workspace.
type WorkspaceRef struct {
	WorkspaceID     string `json:"workspace_id"`
	PathOrURI       string `json:"path_or_uri"`
	ManifestSummary string `json:"manifest_summary,omitempty"`
}

// ---------------------------------------------------------------------------
// Namespaces (spec §6.1)
// ---------------------------------------------------------------------------

// NamespacePolicy holds per-namespace overrides (spec §15.5).
type NamespacePolicy struct {
	TrustedSources          []string       `json:"trusted_sources,omitempty"` // source types allowed to auto-promote (P5)
	TTLOverridesDays        map[string]int `json:"ttl_overrides_days,omitempty"`
	ReviewRequiredCardTypes []string       `json:"review_required_card_types,omitempty"`
	AgentRunRetentionDays   int            `json:"agent_run_retention_days,omitempty"`
}

// Namespace is a hierarchical scope (spec §4.3).
type Namespace struct {
	ID        string          `json:"id"`
	TenantID  string          `json:"tenant_id"`
	ParentID  string          `json:"parent_id,omitempty"`
	Kind      string          `json:"kind"` // org|team|service|repo|incident_family
	Policy    NamespacePolicy `json:"policy"`
	CreatedAt time.Time       `json:"created_at"`
}

// Problem is an RFC 7807 error body (spec §13).
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}
