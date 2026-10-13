// Package store persists the gateway's state in SQLite through the pure-Go
// modernc.org/sqlite driver. All access goes through Queries, which runs
// either directly against the database or inside a transaction (DB.Tx), so
// services can compose several writes into one atomic unit of work.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrConflict is returned on optimistic-lock or uniqueness conflicts.
var ErrConflict = errors.New("store: conflict")

// dbtx is satisfied by both *sql.DB and *sql.Tx.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB owns the SQLite connection.
type DB struct {
	sql *sql.DB
}

// Queries exposes typed data access over a dbtx.
type Queries struct {
	db dbtx
}

// Open opens (creating if needed) the database at path and applies pending
// migrations. A single connection is used: SQLite serialises writers anyway,
// and one connection makes every transaction BEGIN IMMEDIATE without
// SQLITE_BUSY contention between pooled connections.
func Open(ctx context.Context, path string) (*DB, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("store: database path must not contain '?' or '#'")
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)
	db := &DB{sql: sqlDB}
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	if err := db.migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// Ping checks the database is reachable.
func (d *DB) Ping(ctx context.Context) error { return d.sql.PingContext(ctx) }

// Q returns non-transactional queries. Never call it from inside a Tx
// callback: with a single connection that would deadlock.
func (d *DB) Q() *Queries { return &Queries{db: d.sql} }

// Tx runs fn inside a transaction, committing when fn returns nil.
func (d *DB) Tx(ctx context.Context, fn func(q *Queries) error) (err error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(&Queries{db: tx}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// MigrationVersion returns the highest applied migration number.
func (d *DB) MigrationVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := d.sql.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	return int(v.Int64), err
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		var v int
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &v); err != nil {
			return nil, fmt.Errorf("store: bad migration name %q", e.Name())
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := range out {
		if out[i].version != i+1 {
			return nil, fmt.Errorf("store: migrations must be numbered contiguously from 1, got %s", out[i].name)
		}
	}
	return out, nil
}

func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	current, err := d.MigrationVersion(ctx)
	if err != nil {
		return fmt.Errorf("store: read migration version: %w", err)
	}
	if current > len(migs) {
		return fmt.Errorf("store: database schema version %d is newer than this binary (%d)", current, len(migs))
	}
	for _, m := range migs[current:] {
		err := d.Tx(ctx, func(q *Queries) error {
			if _, err := q.db.ExecContext(ctx, m.sql); err != nil {
				return fmt.Errorf("store: migration %s: %w", m.name, err)
			}
			_, err := q.db.ExecContext(ctx,
				`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
				m.version, m.name, time.Now().UnixNano())
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// --- shared helpers -------------------------------------------------------

func ts(t time.Time) int64 { return t.UTC().UnixNano() }

func fromTS(n int64) time.Time { return time.Unix(0, n).UTC() }

func nullTS(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: ts(*t), Valid: true}
}

func fromNullTS(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromTS(n.Int64)
	return &t
}

func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isUnique reports whether err is a SQLite uniqueness/primary-key violation.
func isUnique(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "PRIMARY KEY constraint failed")
}

// notFound maps sql.ErrNoRows to ErrNotFound.
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Page bounds a list query. Results are ordered by id descending (ids are
// time-ordered); StartingAfter is an exclusive cursor.
type Page struct {
	Limit         int
	StartingAfter string
}

func (p Page) limit() int {
	if p.Limit <= 0 || p.Limit > 100 {
		return 100
	}
	return p.Limit
}

// pageClause appends the cursor condition and returns the ORDER/LIMIT tail.
// It fetches one extra row so callers can report has_more.
func pageClause(p Page, where []string, args []any) ([]string, []any, string) {
	if p.StartingAfter != "" {
		where = append(where, "id < ?")
		args = append(args, p.StartingAfter)
	}
	return where, append(args, p.limit()+1), " ORDER BY id DESC LIMIT ?"
}

func whereSQL(where []string) string {
	if len(where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(where, " AND ")
}

// trimPage cuts the look-ahead row and reports whether more rows exist.
func trimPage[T any](items []T, p Page) ([]T, bool) {
	if len(items) > p.limit() {
		return items[:p.limit()], true
	}
	return items, false
}
