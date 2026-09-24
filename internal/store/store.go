// Package store persists items, HTTP cache validators and per-site run state
// in SQLite (§3.1, Store; §9 schema).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure Go: no CGO, easy cross-compiles
)

// Store is safe for concurrent use. There is one writer (the scheduler or
// build); HTTP handlers only read.
type Store struct {
	db *sql.DB
}

// migrations are applied in order; schema_version records how many have run.
var migrations = []string{
	`CREATE TABLE items (
		site_id          TEXT NOT NULL,
		guid             TEXT NOT NULL,
		url              TEXT NOT NULL,              -- the discovered URL, which is fetched
		canonical_url    TEXT NOT NULL,              -- the item link
		title            TEXT NOT NULL,
		summary          TEXT NOT NULL DEFAULT '',
		content_html     TEXT NOT NULL,
		author           TEXT NOT NULL DEFAULT '',
		image_url        TEXT NOT NULL DEFAULT '',
		categories_json  TEXT NOT NULL DEFAULT '[]',
		published_at     INTEGER NOT NULL,           -- Unix ms, frozen on first store (§5.3)
		published_source TEXT NOT NULL,              -- winning source spec, or 'first_seen'
		source_published TEXT NOT NULL DEFAULT '',   -- the source's date (YYYY-MM-DD), to detect real changes
		updated_at       INTEGER,
		first_seen_at    INTEGER NOT NULL,
		last_fetched_at  INTEGER NOT NULL,
		content_hash     TEXT NOT NULL,
		missing_since    INTEGER,
		PRIMARY KEY (site_id, guid)
	);
	CREATE UNIQUE INDEX items_url ON items (site_id, url);
	CREATE INDEX items_recent ON items (site_id, published_at DESC);

	CREATE TABLE http_cache (
		url           TEXT PRIMARY KEY,
		etag          TEXT NOT NULL DEFAULT '',
		last_modified TEXT NOT NULL DEFAULT '',
		fetched_at    INTEGER NOT NULL,
		status        INTEGER NOT NULL
	);

	CREATE TABLE site_state (
		site_id         TEXT PRIMARY KEY,
		last_run_at     INTEGER,
		last_success_at INTEGER,
		last_change_at  INTEGER,                     -- drives <lastBuildDate> (§5.7)
		last_error      TEXT NOT NULL DEFAULT ''
	);`,
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this build supports (%d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Item is one stored article.
type Item struct {
	SiteID          string
	GUID            string
	URL             string
	Canonical       string
	Title           string
	Summary         string
	ContentHTML     string
	Author          string
	ImageURL        string
	Categories      []string
	Published       time.Time
	PublishedSource string
	SourcePublished string
	Updated         time.Time // zero when the item was never edited
	FirstSeen       time.Time
	LastFetched     time.Time
	ContentHash     string
	MissingSince    time.Time // zero while the item is still discovered
}

const itemColumns = `site_id, guid, url, canonical_url, title, summary, content_html, author, image_url,
	categories_json, published_at, published_source, source_published, updated_at, first_seen_at,
	last_fetched_at, content_hash, missing_since`

func scanItem(row interface{ Scan(...any) error }) (*Item, error) {
	var it Item
	var cats string
	var published, firstSeen, lastFetched int64
	var updated, missing sql.NullInt64
	err := row.Scan(&it.SiteID, &it.GUID, &it.URL, &it.Canonical, &it.Title, &it.Summary, &it.ContentHTML,
		&it.Author, &it.ImageURL, &cats, &published, &it.PublishedSource, &it.SourcePublished, &updated,
		&firstSeen, &lastFetched, &it.ContentHash, &missing)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(cats), &it.Categories); err != nil {
		return nil, fmt.Errorf("item %s categories: %w", it.GUID, err)
	}
	it.Published, it.FirstSeen, it.LastFetched = fromMS(published), fromMS(firstSeen), fromMS(lastFetched)
	it.Updated, it.MissingSince = fromNull(updated), fromNull(missing)
	return &it, nil
}

// PutItem inserts or replaces an item.
func (s *Store) PutItem(ctx context.Context, it *Item) error {
	cats, err := json.Marshal(nonNil(it.Categories))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO items (`+itemColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (site_id, guid) DO UPDATE SET
			url = excluded.url, canonical_url = excluded.canonical_url, title = excluded.title,
			summary = excluded.summary, content_html = excluded.content_html, author = excluded.author,
			image_url = excluded.image_url, categories_json = excluded.categories_json,
			published_at = excluded.published_at, published_source = excluded.published_source,
			source_published = excluded.source_published, updated_at = excluded.updated_at,
			first_seen_at = excluded.first_seen_at, last_fetched_at = excluded.last_fetched_at,
			content_hash = excluded.content_hash, missing_since = excluded.missing_since`,
		it.SiteID, it.GUID, it.URL, it.Canonical, it.Title, it.Summary, it.ContentHTML, it.Author, it.ImageURL,
		string(cats), toMS(it.Published), it.PublishedSource, it.SourcePublished, toNull(it.Updated),
		toMS(it.FirstSeen), toMS(it.LastFetched), it.ContentHash, toNull(it.MissingSince))
	return err
}

// ItemByURL returns the item discovered at url, or nil.
func (s *Store) ItemByURL(ctx context.Context, siteID, url string) (*Item, error) {
	it, err := scanItem(s.db.QueryRowContext(ctx, `SELECT `+itemColumns+` FROM items WHERE site_id = ? AND url = ?`, siteID, url))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return it, err
}

// ItemByGUID returns the item with the given guid, or nil.
func (s *Store) ItemByGUID(ctx context.Context, siteID, guid string) (*Item, error) {
	it, err := scanItem(s.db.QueryRowContext(ctx, `SELECT `+itemColumns+` FROM items WHERE site_id = ? AND guid = ?`, siteID, guid))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return it, err
}

// Recent returns up to n items, newest first. Ties (same published time) are
// broken by first-seen time, then guid, so the order is deterministic.
func (s *Store) Recent(ctx context.Context, siteID string, n int) ([]*Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+itemColumns+` FROM items WHERE site_id = ?
		ORDER BY published_at DESC, first_seen_at DESC, guid LIMIT ?`, siteID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Known is the light view of a stored item that run planning needs.
type Known struct {
	GUID         string
	Published    time.Time
	LastFetched  time.Time
	MissingSince time.Time
}

// KnownURLs maps each stored item's discovered URL to its planning fields.
func (s *Store) KnownURLs(ctx context.Context, siteID string) (map[string]Known, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT url, guid, published_at, last_fetched_at, missing_since
		FROM items WHERE site_id = ?`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Known{}
	for rows.Next() {
		var url string
		var k Known
		var published, fetched int64
		var missing sql.NullInt64
		if err := rows.Scan(&url, &k.GUID, &published, &fetched, &missing); err != nil {
			return nil, err
		}
		k.Published, k.LastFetched, k.MissingSince = fromMS(published), fromMS(fetched), fromNull(missing)
		out[url] = k
	}
	return out, rows.Err()
}

// SetMissing marks items that discovery no longer returns (§3.2) and clears
// the mark on items that came back. Items are never deleted here.
func (s *Store) SetMissing(ctx context.Context, siteID string, discovered map[string]bool, now time.Time) error {
	known, err := s.KnownURLs(ctx, siteID)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for url, k := range known {
		switch {
		case discovered[url] && !k.MissingSince.IsZero():
			_, err = tx.ExecContext(ctx, `UPDATE items SET missing_since = NULL WHERE site_id = ? AND url = ?`, siteID, url)
		case !discovered[url] && k.MissingSince.IsZero():
			_, err = tx.ExecContext(ctx, `UPDATE items SET missing_since = ? WHERE site_id = ? AND url = ?`, toMS(now), siteID, url)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TouchFetched records a fetch that found no change (a 304 or identical content).
func (s *Store) TouchFetched(ctx context.Context, siteID, guid string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE items SET last_fetched_at = ? WHERE site_id = ? AND guid = ?`, toMS(at), siteID, guid)
	return err
}

// Validators are the HTTP cache validators from a previous fetch.
type Validators struct {
	ETag         string
	LastModified string
}

// Validators returns the stored validators for url (zero if none).
func (s *Store) Validators(ctx context.Context, url string) (Validators, error) {
	var v Validators
	err := s.db.QueryRowContext(ctx, `SELECT etag, last_modified FROM http_cache WHERE url = ?`, url).Scan(&v.ETag, &v.LastModified)
	if errors.Is(err, sql.ErrNoRows) {
		return Validators{}, nil
	}
	return v, err
}

// PutValidators records the validators and status of a fetch.
func (s *Store) PutValidators(ctx context.Context, url string, v Validators, status int, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO http_cache (url, etag, last_modified, fetched_at, status)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (url) DO UPDATE SET etag = excluded.etag, last_modified = excluded.last_modified,
			fetched_at = excluded.fetched_at, status = excluded.status`,
		url, v.ETag, v.LastModified, toMS(at), status)
	return err
}

// SiteState is the per-site run record.
type SiteState struct {
	LastRun     time.Time
	LastSuccess time.Time
	LastChange  time.Time
	LastError   string
}

// SiteState returns the site's state (zero if it has never run).
func (s *Store) SiteState(ctx context.Context, siteID string) (SiteState, error) {
	var st SiteState
	var run, ok, change sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT last_run_at, last_success_at, last_change_at, last_error
		FROM site_state WHERE site_id = ?`, siteID).Scan(&run, &ok, &change, &st.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return SiteState{}, nil
	}
	st.LastRun, st.LastSuccess, st.LastChange = fromNull(run), fromNull(ok), fromNull(change)
	return st, err
}

// PutSiteState replaces the site's state.
func (s *Store) PutSiteState(ctx context.Context, siteID string, st SiteState) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO site_state (site_id, last_run_at, last_success_at, last_change_at, last_error)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (site_id) DO UPDATE SET last_run_at = excluded.last_run_at,
			last_success_at = excluded.last_success_at, last_change_at = excluded.last_change_at,
			last_error = excluded.last_error`,
		siteID, toNull(st.LastRun), toNull(st.LastSuccess), toNull(st.LastChange), st.LastError)
	return err
}

func toMS(t time.Time) int64 { return t.UnixMilli() }

func fromMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func toNull(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func fromNull(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return fromMS(v.Int64)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
