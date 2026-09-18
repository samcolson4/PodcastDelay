// Package store is the SQLite persistence layer: embedded migrations
// plus typed queries for subscriptions and episodes. Every method is
// available both directly on a Store (auto-committing) and inside a
// transaction via Store.WithTx, so callers that need multiple writes
// to land atomically (the refresh loop) get one commit per subscription.
package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// dbtx is satisfied by both *sql.DB and *sql.Tx.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Queries is the set of typed operations, usable against either a plain
// connection or a transaction.
type Queries struct {
	db dbtx
}

func newQueries(db dbtx) *Queries { return &Queries{db: db} }

// Store owns the underlying connection pool and embeds Queries so
// Store methods run each as its own implicit transaction.
type Store struct {
	*Queries
	db *sql.DB
}

// Open opens (creating if necessary) the SQLite database at path and
// applies any pending embedded migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite only supports one writer at a time; a single connection
	// avoids SQLITE_BUSY under our own load rather than papering over
	// it with retries.
	db.SetMaxOpenConns(1)

	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{Queries: newQueries(db), db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// WithTx runs fn inside a transaction, committing on success and
// rolling back if fn returns an error or panics.
func (s *Store) WithTx(ctx context.Context, fn func(q *Queries) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
		if err != nil {
			tx.Rollback()
			return
		}
		err = tx.Commit()
	}()

	err = fn(newQueries(tx))
	return err
}

// Ping verifies the database connection is alive, for the healthz route.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}
