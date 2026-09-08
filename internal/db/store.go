// This file is hand-written and sits alongside the sqlc-generated code in the
// same package. It owns the connection pool, transaction plumbing, and the
// translation of Postgres error codes into conditions the domain packages can
// branch on.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig is the subset of configuration the data layer needs, passed in so
// this package does not import the config package.
type PoolConfig struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

// Store owns the pgx pool and exposes the generated queries. A Store is safe
// for concurrent use.
type Store struct {
	*Queries
	pool *pgxpool.Pool
}

// Open connects to Postgres and verifies the connection before returning, so a
// bad DATABASE_URL fails at boot rather than on the first request.
func Open(ctx context.Context, cfg PoolConfig) (*Store, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &Store{Queries: New(pool), pool: pool}, nil
}

// Pool exposes the underlying pool for health checks and metrics.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases all pooled connections.
func (s *Store) Close() { s.pool.Close() }

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// InTx runs fn inside a single transaction, committing on success and rolling
// back on any error or panic. This is the only way multi-statement work in this
// system reaches the database: the booking flow depends on every spot
// reservation and the booking row itself succeeding or failing together.
func (s *Store) InTx(ctx context.Context, fn func(*Queries) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	// Rollback after a successful commit is a no-op, so the deferred call is
	// safe on every path and guarantees no connection is left in a
	// transaction if fn panics.
	defer func() {
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()

	if err := fn(s.Queries.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// Postgres error codes this system branches on.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgCheckViolation      = "23514"
	pgSerializationFail   = "40001"
	pgDeadlockDetected    = "40P01"
)

// IsNoRows reports whether err is pgx's empty-result sentinel. A :one query
// that returns nothing is a normal outcome for the conditional writes in this
// system — it means the guard clause rejected the operation — so callers treat
// it as a domain condition rather than a failure.
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// pgError extracts the Postgres error, if any.
func pgError(err error) *pgconn.PgError {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr
	}
	return nil
}

// IsUniqueViolation reports whether err is a unique constraint violation, and
// optionally whether it names one of the given constraints. Naming the
// constraint matters on tables with several unique indexes: a payment insert
// can collide on the transaction reference (a reused receipt) or on the
// idempotency key (a retried request), and those mean opposite things to the
// caller.
func IsUniqueViolation(err error, constraints ...string) bool {
	pgErr := pgError(err)
	if pgErr == nil || pgErr.Code != pgUniqueViolation {
		return false
	}
	if len(constraints) == 0 {
		return true
	}
	for _, name := range constraints {
		if pgErr.ConstraintName == name {
			return true
		}
	}
	return false
}

// ConstraintName returns the constraint a failed write violated, or "".
func ConstraintName(err error) string {
	if pgErr := pgError(err); pgErr != nil {
		return pgErr.ConstraintName
	}
	return ""
}

// IsForeignKeyViolation reports whether err references a missing parent row,
// such as a booking pointing at a game that was deleted.
func IsForeignKeyViolation(err error) bool {
	pgErr := pgError(err)
	return pgErr != nil && pgErr.Code == pgForeignKeyViolation
}

// IsCheckViolation reports whether err violated a CHECK constraint. These are
// invariant breaches — a total that does not equal subtotal minus discount, or
// spots_remaining above capacity — and indicate a bug rather than bad input.
func IsCheckViolation(err error) bool {
	pgErr := pgError(err)
	return pgErr != nil && pgErr.Code == pgCheckViolation
}

// IsRetryable reports whether err is a transient concurrency failure that the
// caller may retry as-is. Serialization failures and deadlocks are the two
// cases; both leave no partial work behind because the transaction was rolled
// back by Postgres.
func IsRetryable(err error) bool {
	pgErr := pgError(err)
	if pgErr == nil {
		return false
	}
	return pgErr.Code == pgSerializationFail || pgErr.Code == pgDeadlockDetected
}

// Interval converts a Go duration into the Postgres interval pgx expects, for
// the queries that express a window rather than an absolute instant.
func Interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
