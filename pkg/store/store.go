// Package store persists discovery results in SQLite.
//
// Two properties matter more than anything else here:
//
// Idempotency. Running discovery twice must not produce two of anything. That is enforced
// by database constraints rather than by application care — see company_identities in
// migrations/001_initial.sql — because a constraint holds even when a run crashes halfway.
//
// A private schema stays private. The engine owns the discovery tables. A private consumer
// adds application drafts, sent applications and suppression lists under its own migration
// namespace against the same file, so personal data never needs a table defined in this
// public repository.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no CGO, so static cross-compilation stays trivial
)

//go:embed migrations/*.sql
var engineMigrations embed.FS

// EngineNamespace labels the migrations owned by this package. A private consumer uses its
// own namespace so the two sets of migrations never renumber each other.
const EngineNamespace = "eucd"

// MigrationSet is one owner's ordered migrations.
type MigrationSet struct {
	// Namespace distinguishes this owner's version numbers from everyone else's.
	Namespace string
	// FS holds the .sql files.
	FS fs.FS
	// Dir is the directory within FS to read. Empty means the root.
	Dir string
}

// EngineMigrations returns the discovery schema owned by this package.
func EngineMigrations() MigrationSet {
	return MigrationSet{Namespace: EngineNamespace, FS: engineMigrations, Dir: "migrations"}
}

// Store is a handle on the database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens or creates the database at path and configures it for this workload.
//
// Pass ":memory:" for a throwaway database in tests.
func Open(ctx context.Context, dbPath string) (*Store, error) {
	if dbPath == "" {
		return nil, errors.New("no database path given")
	}
	dsn := dbPath
	if dbPath == ":memory:" {
		// A plain ":memory:" database is per-connection, and database/sql pools
		// connections, so half the queries would land on an empty database. A shared
		// cache keeps one in-memory database behind the whole pool.
		dsn = "file::memory:?cache=shared"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", dbPath, err)
	}
	// SQLite takes one writer at a time. Letting the pool open several write connections
	// turns contention into "database is locked" errors instead of queueing, so the pool is
	// held to one connection and the work is serialised here.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	pragmas := []string{
		"PRAGMA journal_mode = WAL",   // readers do not block the writer
		"PRAGMA synchronous = NORMAL", // durable enough for a crawl, much faster than FULL
		"PRAGMA foreign_keys = ON",    // the identity and provenance constraints must bite
		"PRAGMA busy_timeout = 10000", // wait for a lock rather than failing instantly
		"PRAGMA temp_store = MEMORY",
	}
	for _, pragma := range pragmas {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close() // cleanup; the pragma failure below is what explains the error
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	return &Store{db: db, path: dbPath}, nil
}

// DB exposes the handle so a private consumer can run its own queries and migrations
// against the same database without this package knowing its schema.
func (s *Store) DB() *sql.DB { return s.db }

// Path reports where the database lives.
func (s *Store) Path() string { return s.path }

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

type migration struct {
	namespace  string
	version    int
	name       string
	statements string
}

// Migrate brings the database up to date for every set given, in the order given.
//
// Each migration runs inside its own transaction together with the row that records it, so
// a migration either applied completely or not at all. Already-applied versions are skipped,
// which is what lets this be called unconditionally on every start.
func (s *Store) Migrate(ctx context.Context, sets ...MigrationSet) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		    namespace  TEXT NOT NULL,
		    version    INTEGER NOT NULL,
		    name       TEXT NOT NULL,
		    applied_at TEXT NOT NULL,
		    PRIMARY KEY (namespace, version)
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	for _, set := range sets {
		pending, err := s.pending(ctx, set)
		if err != nil {
			return err
		}
		for _, m := range pending {
			if err := s.apply(ctx, m); err != nil {
				return fmt.Errorf("migration %s/%03d_%s: %w", m.namespace, m.version, m.name, err)
			}
		}
	}
	return nil
}

func (s *Store) pending(ctx context.Context, set MigrationSet) ([]migration, error) {
	if set.Namespace == "" {
		return nil, errors.New("migration set has no namespace")
	}
	dir := set.Dir
	if dir == "" {
		dir = "."
	}
	entries, err := fs.ReadDir(set.FS, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations for %q: %w", set.Namespace, err)
	}

	applied, err := s.appliedVersions(ctx, set.Namespace)
	if err != nil {
		return nil, err
	}

	var pending []migration
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("%s/%s: %w", set.Namespace, entry.Name(), err)
		}
		if applied[version] {
			continue
		}
		body, err := fs.ReadFile(set.FS, path.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		pending = append(pending, migration{
			namespace: set.Namespace, version: version, name: name, statements: string(body),
		})
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].version < pending[j].version })
	return pending, nil
}

func (s *Store) appliedVersions(ctx context.Context, namespace string) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT version FROM schema_migrations WHERE namespace = ?`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	applied := map[int]bool{}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		applied[version] = true
	}
	return applied, rows.Err()
}

// parseMigrationName reads "003_add_contacts.sql" as version 3, name "add_contacts".
func parseMigrationName(filename string) (int, string, error) {
	base := strings.TrimSuffix(filename, ".sql")
	prefix, rest, found := strings.Cut(base, "_")
	if !found {
		return 0, "", errors.New(`name must be "<version>_<description>.sql", e.g. "001_initial.sql"`)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("%q is not a positive version number", prefix)
	}
	return version, rest, nil
}

func (s *Store) apply(ctx context.Context, m migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a committed tx rolls back to a no-op

	if _, err := tx.ExecContext(ctx, m.statements); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (namespace, version, name, applied_at) VALUES (?, ?, ?, ?)`,
		m.namespace, m.version, m.name, nowRFC3339()); err != nil {
		return err
	}
	return tx.Commit()
}

// AppliedMigrations lists what has been applied, newest first, for `eucd db status`.
func (s *Store) AppliedMigrations(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT namespace, version, name, applied_at FROM schema_migrations
		 ORDER BY namespace, version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var namespace, name, appliedAt string
		var version int
		if err := rows.Scan(&namespace, &version, &name, &appliedAt); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s/%03d_%s (applied %s)", namespace, version, name, appliedAt))
	}
	return out, rows.Err()
}

// Counts returns row counts for the tables a human wants to see in `eucd db status`.
func (s *Store) Counts(ctx context.Context) (map[string]int, error) {
	tables := []string{
		"associations", "companies", "company_identities", "company_sources",
		"contacts", "crawl_runs", "professional_profiles", "company_rankings",
	}
	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		// The table list is a constant in this file, never user input.
		err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count)
		if err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		counts[table] = count
	}
	return counts, nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// Backup writes a consistent copy of the database to destination while the engine may still
// be running, using SQLite's own VACUUM INTO. A file copy of a live WAL database can be
// torn; this cannot.
func (s *Store) Backup(ctx context.Context, destination string) error {
	if destination == "" {
		return errors.New("no backup destination given")
	}
	if strings.ContainsAny(destination, "'\x00") {
		return fmt.Errorf("backup path %q contains a quote or null byte", destination)
	}
	// VACUUM INTO takes a literal, not a bound parameter.
	_, err := s.db.ExecContext(ctx, "VACUUM INTO '"+destination+"'")
	if err != nil {
		return fmt.Errorf("backup to %q: %w", destination, err)
	}
	return nil
}
