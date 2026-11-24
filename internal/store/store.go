// Package store keeps cestafet's shared state in a local SQLite database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// Store is a handle on the cestafet database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id          TEXT PRIMARY KEY,
	claude_pid  INTEGER NOT NULL DEFAULT 0,
	profile     TEXT NOT NULL,
	project     TEXT NOT NULL,
	repo        TEXT NOT NULL DEFAULT '',
	cwd         TEXT NOT NULL,
	branch      TEXT NOT NULL DEFAULT '',
	summary     TEXT NOT NULL DEFAULT '',
	role        TEXT NOT NULL DEFAULT 'member',
	status      TEXT NOT NULL DEFAULT 'live',
	started_at  INTEGER NOT NULL,
	last_seen   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_project ON sessions(project, status);
CREATE INDEX IF NOT EXISTS sessions_pid ON sessions(claude_pid, status);

CREATE TABLE IF NOT EXISTS prompts (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id  TEXT NOT NULL,
	text        TEXT NOT NULL,
	at          INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS prompts_session ON prompts(session_id, id);

CREATE TABLE IF NOT EXISTS notices (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	project       TEXT NOT NULL,
	from_session  TEXT NOT NULL,
	from_profile  TEXT NOT NULL,
	from_role     TEXT NOT NULL,
	origin        TEXT NOT NULL,
	prompt_id     INTEGER,
	body          TEXT NOT NULL,
	created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS notices_project ON notices(project, created_at);

CREATE TABLE IF NOT EXISTS deliveries (
	notice_id     INTEGER NOT NULL,
	to_session    TEXT NOT NULL,
	delivered_at  INTEGER,
	acked_at      INTEGER,
	reply         TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (notice_id, to_session)
);
CREATE INDEX IF NOT EXISTS deliveries_pending ON deliveries(to_session, delivered_at);
`

// migrations upgrade a database step by step; PRAGMA user_version records the
// last step applied. Index i is the step from version i to i+1. The base schema
// above stays the v0.1.0 one, so new and old databases take the same path.
var migrations = []string{
	// v0.2: standing rules.
	`ALTER TABLE notices ADD COLUMN kind TEXT NOT NULL DEFAULT 'info';
	ALTER TABLE notices ADD COLUMN expires_at INTEGER;
	ALTER TABLE notices ADD COLUMN lifted_at INTEGER;
	ALTER TABLE notices ADD COLUMN lifted_by TEXT;
	ALTER TABLE notices ADD COLUMN lifts INTEGER;
	CREATE INDEX IF NOT EXISTS notices_rules ON notices(project, kind, lifted_at);`,
	// v0.2: guard patterns on rules, newline-separated.
	`ALTER TABLE notices ADD COLUMN guards TEXT NOT NULL DEFAULT '';`,
	// Windows support: the owning claude process's creation time (unix ms, 0 = unknown), so a
	// reused pid is not taken for the same process.
	`ALTER TABLE sessions ADD COLUMN claude_started INTEGER NOT NULL DEFAULT 0;`,
}

func isBusy(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "locked") || strings.Contains(s, "busy")
}

// enableWAL ensures the database uses WAL journaling. Switching a brand-new
// database from rollback to WAL requires an exclusive lock and can briefly
// return SQLITE_BUSY when hooks and the MCP server open it at the same time.
func enableWAL(db *sql.DB) error {
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err == nil && strings.EqualFold(mode, "wal") {
		return nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&mode)
		if err == nil {
			return nil
		}
		if !isBusy(err) || time.Now().After(deadline) {
			return fmt.Errorf("enable wal: %w", err)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// withImmediateTx runs fn inside a BEGIN IMMEDIATE transaction on a dedicated
// connection so concurrent readers cannot observe intermediate state or race
// on a deferred read-to-write lock upgrade.
func withImmediateTx(db *sql.DB, fn func(*sql.Conn) error) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(conn); err != nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// initSchema applies the base schema and any pending migrations atomically
// under BEGIN IMMEDIATE so concurrent first opens never race on ALTER TABLE.
func initSchema(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err == nil && version >= len(migrations) {
		return nil
	}
	return withImmediateTx(db, func(conn *sql.Conn) error {
		ctx := context.Background()
		var v int
		if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
		if v >= len(migrations) {
			return nil
		}
		if _, err := conn.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		for ; v < len(migrations); v++ {
			if _, err := conn.ExecContext(ctx, migrations[v]); err != nil {
				return fmt.Errorf("migrate to %d: %w", v+1, err)
			}
			if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
				return fmt.Errorf("migrate to %d: %w", v+1, err)
			}
		}
		return nil
	})
}

// DefaultPath returns the database location: $CESTAFET_DB, else
// $XDG_DATA_HOME/cestafet/bus.db, else %USERPROFILE%\.cestafet\bus.db on
// Windows (outside AppData, which the Claude desktop app's package redirects)
// and ~/.local/share/cestafet/bus.db elsewhere.
func DefaultPath() (string, error) {
	if p := os.Getenv("CESTAFET_DB"); p != "" {
		return p, nil
	}
	if base := os.Getenv("XDG_DATA_HOME"); base != "" {
		return filepath.Join(base, "cestafet", "bus.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(home, ".cestafet", "bus.db"), nil
	}
	return filepath.Join(home, ".local", "share", "cestafet", "bus.db"), nil
}

// Open opens (and creates if needed) the database at path. The directory is
// created with mode 0700 and the file with mode 0600.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create database file: %w", err)
	}
	_ = f.Close()

	dsn, err := fileURI(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := enableWAL(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := initSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

// fileURI builds the SQLite URI for a database file. The path is made absolute
// and percent-encoded, so spaces, '#', '%' and '?' in a home directory (common
// on Windows) cannot cut the path short or be read as URI syntax.
func fileURI(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("database path: %w", err)
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // C:/Users/... → /C:/Users/...
	}
	u := url.URL{Scheme: "file", Path: p,
		RawQuery: "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"}
	return u.String(), nil
}

// OpenDefault opens the database at DefaultPath.
func OpenDefault() (*Store, error) {
	p, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return Open(p)
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// SetClock replaces the time source; used by tests.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

func (s *Store) unix() int64 { return s.now().Unix() }

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")
