// Package store persists follows and posts in SQLite.
//
// Times are stored as INTEGER unix milliseconds in UTC (0 is the zero time),
// so every time.Time read back is UTC and truncated to the millisecond.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite" // also registers the "sqlite" driver
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrDuplicate = errors.New("a follow with this feed URL already exists")
)

// readerConns bounds the read pool; WAL readers never block each other or the writer.
const readerConns = 4

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store wraps the SQLite database. Safe for concurrent use.
//
// All writes go through a single-connection pool so concurrent writers queue
// inside database/sql instead of racing for SQLite's write lock, while a
// separate read-only pool serves queries against WAL snapshots.
type Store struct {
	w   *sql.DB
	r   *sql.DB
	now func() time.Time
}

// Open creates the parent directory if needed, opens the database (WAL,
// foreign keys, busy timeout) and applies embedded migrations.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return nil, fmt.Errorf("store: create database directory: %w", err)
	}

	w, err := sql.Open("sqlite", dsn(abs, false))
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	w.SetMaxOpenConns(1)
	s := &Store{w: w, now: time.Now}
	if err := s.migrateWithRetry(context.Background()); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("store: migrate %s: %w", abs, err)
	}

	// opened after migrating so readers always find the database in WAL mode
	r, err := sql.Open("sqlite", dsn(abs, true))
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("store: open: %w", err)
	}
	r.SetMaxOpenConns(readerConns)
	r.SetMaxIdleConns(readerConns)
	if err := r.Ping(); err != nil {
		_ = errors.Join(r.Close(), w.Close())
		return nil, fmt.Errorf("store: open: %w", err)
	}
	s.r = r
	return s, nil
}

// dsn builds a file: URI so any character in path survives escaping.
func dsn(path string, readOnly bool) string {
	q := url.Values{}
	// busy_timeout first so the WAL switch on a fresh file already waits for locks
	for _, p := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		// takes the write lock at BEGIN, where busy_timeout applies, instead of failing on upgrade
		q.Set("_txlock", "immediate")
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: q.Encode()}
	return u.String()
}

func (s *Store) Close() error {
	var errs []error
	if s.r != nil {
		errs = append(errs, s.r.Close())
	}
	errs = append(errs, s.w.Close())
	return errors.Join(errs...)
}

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads migrations/NNNN_name.sql, requiring versions 1..n without gaps.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	ms := make([]migration, 0, len(entries))
	for i, e := range entries {
		prefix, _, _ := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if err != nil || v != i+1 {
			return nil, fmt.Errorf("migration %s: want version %d", e.Name(), i+1)
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		ms = append(ms, migration{version: v, name: e.Name(), sql: string(b)})
	}
	return ms, nil
}

// migrateWithRetry retries migrate on SQLITE_BUSY for a few seconds: concurrent
// first opens can deadlock switching a new file to WAL, and SQLite reports
// that immediately instead of waiting for busy_timeout.
func (s *Store) migrateWithRetry(ctx context.Context) error {
	deadline := time.Now().Add(5 * time.Second)
	for wait := 10 * time.Millisecond; ; wait = min(2*wait, 250*time.Millisecond) {
		err := s.migrate(ctx)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		// jitter so racing processes do not retry in lockstep
		time.Sleep(wait + rand.N(wait))
	}
}

func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

// migrate applies pending migrations one transaction each. The version is
// re-read inside every transaction so two processes opening the same new
// database cannot both apply a migration.
func (s *Store) migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	for {
		done := false
		err := s.inTx(ctx, func(tx *sql.Tx) error {
			var current int
			if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
				return fmt.Errorf("read schema version: %w", err)
			}
			if current > len(ms) {
				return fmt.Errorf("database schema version %d is newer than this build supports (%d)", current, len(ms))
			}
			if current == len(ms) {
				done = true
				return nil
			}
			m := ms[current]
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return fmt.Errorf("migration %s: %w", m.name, err)
			}
			// PRAGMA takes no bind parameters; m.version is an int
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
				return fmt.Errorf("migration %s: %w", m.name, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// inTx runs fn in a write transaction and commits when it returns nil.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// a no-op after Commit; on error or panic it releases the only writer connection
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// GetSetting returns def when the key is not set.
func (s *Store) GetSetting(ctx context.Context, key, def string) (string, error) {
	var v string
	err := s.r.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return def, nil
	case err != nil:
		return "", fmt.Errorf("store: get setting %q: %w", key, err)
	}
	return v, nil
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.w.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("store: set setting %q: %w", key, err)
	}
	return nil
}

func toMS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// asStored rounds t the way a store round trip does.
func asStored(t time.Time) time.Time { return fromMS(toMS(t)) }

// msTime scans an INTEGER unix-ms column into a time.Time.
type msTime struct{ t *time.Time }

func (m msTime) Scan(src any) error {
	v, ok := src.(int64)
	if !ok {
		return fmt.Errorf("store: time column holds %T, want int64", src)
	}
	*m.t = fromMS(v)
	return nil
}

// jsonStrings scans a JSON array of strings, leaving nil for an empty array.
type jsonStrings struct{ v *[]string }

func (j jsonStrings) Scan(src any) error {
	var b []byte
	switch x := src.(type) {
	case nil:
		*j.v = nil
		return nil
	case string:
		b = []byte(x)
	case []byte:
		b = x
	default:
		return fmt.Errorf("store: JSON column holds %T", src)
	}
	var out []string
	if err := json.Unmarshal(b, &out); err != nil {
		return fmt.Errorf("store: decode JSON column: %w", err)
	}
	if len(out) == 0 {
		out = nil
	}
	*j.v = out
	return nil
}
