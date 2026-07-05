package postgres

import "github.com/lazorfuzz/memba/internal/store"

// Compile-time contract check.
var _ store.Store = (*PG)(nil)
