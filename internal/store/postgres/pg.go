// Package postgres implements store.Store on PostgreSQL 16 + pgvector +
// pg_trgm (spec §4.1). Every statement carries an explicit tenant_id
// predicate; there is no query path without one (spec §4.3).
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/lazorfuzz/memba/migrations"
	"github.com/lazorfuzz/memba/pkg/memory"
)

type PG struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, dsn string, maxConns int) (*PG, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = int32(maxConns)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &PG{pool: pool}, nil
}

func (p *PG) Close() { p.pool.Close() }

func (p *PG) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// Migrate applies embedded goose migrations.
func Migrate(dsn string) error {
	db := stdlib.OpenDB(*mustParse(dsn))
	defer db.Close()
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(db, ".")
}

func mustParse(dsn string) *pgx.ConnConfig {
	c, err := pgx.ParseConfig(dsn)
	if err != nil {
		panic(err)
	}
	return c
}

// --- helpers ---------------------------------------------------------------

func jsonb(v any) []byte {
	if v == nil {
		return []byte("{}")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func aclJSON(a memory.ACL) []byte {
	if a.Read == nil {
		a.Read = []string{}
	}
	return jsonb(a)
}

func parseACL(b []byte) memory.ACL {
	var a memory.ACL
	_ = json.Unmarshal(b, &a)
	return a
}

func parseMeta(b []byte) map[string]any {
	if len(b) == 0 {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func nullTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	tt := t.Time
	return &tt
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullInt(i int) sql.NullInt32 {
	if i == 0 {
		return sql.NullInt32{}
	}
	return sql.NullInt32{Int32: int32(i), Valid: true}
}
