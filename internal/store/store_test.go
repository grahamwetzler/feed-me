package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMigrateAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "rss-er.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 24, 12, 0, 0, 123e6, time.UTC)
	it := &Item{
		SiteID: "s", GUID: "g", URL: "https://e.com/a", Canonical: "https://e.com/a", Title: "T",
		ContentHTML: "<p>x</p>", Categories: []string{"a", "b"}, Published: now, PublishedSource: "meta:x",
		FirstSeen: now, LastFetched: now, ContentHash: "h",
	}
	if err := s.PutItem(ctx, it); err != nil {
		t.Fatal(err)
	}
	it.Title = "T2"
	if err := s.PutItem(ctx, it); err != nil { // upsert
		t.Fatal(err)
	}
	s.Close()

	// Reopening must not rerun migrations.
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.ItemByURL(ctx, "s", "https://e.com/a")
	if err != nil || got == nil {
		t.Fatalf("%v %v", got, err)
	}
	if got.Title != "T2" || !got.Published.Equal(now) || strings.Join(got.Categories, ",") != "a,b" || !got.Updated.IsZero() {
		t.Errorf("round trip: %+v", got)
	}
	if missing, _ := s.ItemByURL(ctx, "s", "https://e.com/nope"); missing != nil {
		t.Error("want nil for unknown URL")
	}

	if err := s.PutValidators(ctx, it.URL, Validators{ETag: `"e"`}, 200, now); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Validators(ctx, it.URL); v.ETag != `"e"` {
		t.Errorf("validators: %+v", v)
	}
	if err := s.PutSiteState(ctx, "s", SiteState{LastRun: now, LastError: "boom"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.SiteState(ctx, "s"); !st.LastRun.Equal(now) || st.LastError != "boom" || !st.LastChange.IsZero() {
		t.Errorf("state: %+v", st)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rss-er.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE schema_version SET version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "newer than this build") {
		t.Errorf("err = %v", err)
	}
}

func TestPathWithURICharacters(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "odd?name#100%.db")
	for i := 0; i < 2; i++ { // create, then reopen
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := s.PutSiteState(ctx, "s", SiteState{LastError: "x"}); err != nil {
				t.Fatal(err)
			}
		} else if st, _ := s.SiteState(ctx, "s"); st.LastError != "x" {
			t.Errorf("reopened a different database: %+v", st)
		}
		var mode string
		if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
			t.Errorf("journal_mode = %q, %v; pragmas were dropped", mode, err)
		}
		s.Close()
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "odd?name#100%.db") {
			t.Errorf("unexpected file %q", e.Name())
		}
	}
}
