// Package config loads configs/memd.yaml (spec §4.4). Every query logs
// config_hash = sha256 of the effective retrieval+model config (I5).
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Server struct {
	HTTPAddr  string `yaml:"http_addr" json:"http_addr"`
	MaxBodyMB int    `yaml:"max_body_mb" json:"max_body_mb"`
}

type Postgres struct {
	DSNEnv   string `yaml:"dsn_env" json:"dsn_env"`
	MaxConns int    `yaml:"max_conns" json:"max_conns"`
}

type ObjStore struct {
	// Backend is "fs" (default, dev) or "s3". The S3 backend reads endpoint
	// from EndpointEnv; the fs backend writes under Dir.
	Backend     string `yaml:"backend" json:"backend"`
	Dir         string `yaml:"dir" json:"dir"`
	EndpointEnv string `yaml:"endpoint_env" json:"endpoint_env"`
	Bucket      string `yaml:"bucket" json:"bucket"`
}

type ModelRef struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
	Dim      int    `yaml:"dim,omitempty" json:"dim,omitempty"`
	Output   string `yaml:"output,omitempty" json:"output,omitempty"`
	MaxPairs int    `yaml:"max_pairs,omitempty" json:"max_pairs,omitempty"`
}

type Models struct {
	Embedder  ModelRef `yaml:"embedder" json:"embedder"`
	Reranker  ModelRef `yaml:"reranker" json:"reranker"`
	Extractor ModelRef `yaml:"extractor" json:"extractor"`
	Judge     ModelRef `yaml:"judge" json:"judge"`
}

type Budgets struct {
	Boot           int `yaml:"boot" json:"boot"`
	Scoped         int `yaml:"scoped" json:"scoped"`
	Deep           int `yaml:"deep" json:"deep"`
	WorkspaceFiles int `yaml:"workspace_files" json:"workspace_files"`
}

type Retrieval struct {
	PerRetrieverN int     `yaml:"per_retriever_n" json:"per_retriever_n"`
	RRFK          int     `yaml:"rrf_k" json:"rrf_k"`
	FusedPool     int     `yaml:"fused_pool" json:"fused_pool"`
	RerankTop     int     `yaml:"rerank_top" json:"rerank_top"`
	Budgets       Budgets `yaml:"budgets" json:"budgets"`
}

type Lifecycle struct {
	TTLDays                map[string]int `yaml:"ttl_days" json:"ttl_days"`
	DormantAfterTTLMultiple int           `yaml:"dormant_after_ttl_multiple" json:"dormant_after_ttl_multiple"`
	DedupCosine            float64        `yaml:"dedup_cosine" json:"dedup_cosine"`
}

type Promotion struct {
	MinSourcesForMultiSourceRule int  `yaml:"min_sources_for_multi_source_rule" json:"min_sources_for_multi_source_rule"`
	ChatOnlyCanPromote           bool `yaml:"chat_only_can_promote" json:"chat_only_can_promote"`
}

type InjectionScan struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Action  string `yaml:"action" json:"action"` // quarantine
}

type SecretScan struct {
	Ruleset          string  `yaml:"ruleset" json:"ruleset"`
	EntropyThreshold float64 `yaml:"entropy_threshold" json:"entropy_threshold"`
}

type Security struct {
	InjectionScan     InjectionScan `yaml:"injection_scan" json:"injection_scan"`
	SecretScan        SecretScan    `yaml:"secret_scan" json:"secret_scan"`
	WorkspaceTTLHours int           `yaml:"workspace_ttl_hours" json:"workspace_ttl_hours"`
	// TokenSecretEnv names the env var holding the HMAC key for bearer tokens.
	TokenSecretEnv string `yaml:"token_secret_env" json:"token_secret_env"`
}

type Verify struct {
	// RepoRoot is the directory under which verification looks for local
	// clones named <repo> (checkout cache root, §9.4).
	RepoRoot string `yaml:"repo_root" json:"repo_root"`
	Workers  int    `yaml:"workers" json:"workers"`
}

type Config struct {
	Server    Server    `yaml:"server" json:"server"`
	Postgres  Postgres  `yaml:"postgres" json:"postgres"`
	ObjStore  ObjStore  `yaml:"objstore" json:"objstore"`
	Models    Models    `yaml:"models" json:"models"`
	Retrieval Retrieval `yaml:"retrieval" json:"retrieval"`
	Lifecycle Lifecycle `yaml:"lifecycle" json:"lifecycle"`
	Promotion Promotion `yaml:"promotion" json:"promotion"`
	Security  Security  `yaml:"security" json:"security"`
	Verify    Verify    `yaml:"verify" json:"verify"`
}

// Default returns the shipping defaults from spec §4.4.
func Default() Config {
	return Config{
		Server:   Server{HTTPAddr: ":8080", MaxBodyMB: 32},
		Postgres: Postgres{DSNEnv: "MEMD_PG_DSN", MaxConns: 32},
		ObjStore: ObjStore{Backend: "fs", Dir: "./data/objstore", EndpointEnv: "MEMD_S3_ENDPOINT", Bucket: "memba"},
		Models: Models{
			Embedder:  ModelRef{Provider: "local", Model: "hash-embedder-v1", Dim: 1024, Output: "halfvec"},
			Reranker:  ModelRef{Provider: "local", Model: "lexical-overlap-v1", MaxPairs: 100},
			Extractor: ModelRef{Provider: "anthropic", Model: "claude-sonnet-latest"},
			Judge:     ModelRef{Provider: "anthropic", Model: "claude-sonnet-latest"},
		},
		Retrieval: Retrieval{
			PerRetrieverN: 50, RRFK: 60, FusedPool: 150, RerankTop: 100,
			Budgets: Budgets{Boot: 700, Scoped: 4000, Deep: 12000, WorkspaceFiles: 64000},
		},
		Lifecycle: Lifecycle{
			TTLDays:                map[string]int{"code": 28, "operational": 90, "design": 180},
			DormantAfterTTLMultiple: 2,
			DedupCosine:            0.92,
		},
		Promotion: Promotion{MinSourcesForMultiSourceRule: 2, ChatOnlyCanPromote: false},
		Security: Security{
			InjectionScan:     InjectionScan{Enabled: true, Action: "quarantine"},
			SecretScan:        SecretScan{Ruleset: "gitleaks-default", EntropyThreshold: 4.5},
			WorkspaceTTLHours: 72,
			TokenSecretEnv:    "MEMD_TOKEN_SECRET",
		},
		Verify: Verify{RepoRoot: "./data/repos", Workers: 8},
	}
}

// Load reads a YAML config file over the defaults. A missing path returns
// pure defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// Hash returns the config_hash logged on every query (I5): sha256 over the
// effective retrieval + model config, deterministically serialized.
func (c Config) Hash() string {
	sub := struct {
		Models    Models    `json:"models"`
		Retrieval Retrieval `json:"retrieval"`
		Lifecycle Lifecycle `json:"lifecycle"`
		Promotion Promotion `json:"promotion"`
	}{c.Models, c.Retrieval, c.Lifecycle, c.Promotion}
	b, _ := json.Marshal(sub)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TTL returns the verification TTL for a ttl_class (spec §10.7).
func (c Config) TTL(ttlClass string) time.Duration {
	days, ok := c.Lifecycle.TTLDays[ttlClass]
	if !ok {
		days = 28
	}
	return time.Duration(days) * 24 * time.Hour
}
