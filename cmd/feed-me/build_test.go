package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rssfeed "feed-me/internal/feed"
)

func TestCheckRejectsBadLimit(t *testing.T) {
	fx := fixtures(t)
	transport, rateOverride = fx, 1000
	t.Cleanup(func() { transport, rateOverride = nil, 0 })
	var stdout, stderr bytes.Buffer
	code := run([]string{"check", "--config", root + "/feed-me.yaml", "--site", "select-dev", "--limit", "-1"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--limit must be at least 1") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
	if n := len(fx.Requests); n != 0 {
		t.Errorf("made %d requests before rejecting the flag", n)
	}
}

// Most sitemap URLs have no fixture and 404, so the run stores the saved
// posts and records errors for the rest: the feed is written, but the exit
// status reports the failures.
func TestBuildWritesFeedButFailsOnItemErrors(t *testing.T) {
	transport, rateOverride = fixtures(t), 1000
	t.Cleanup(func() { transport, rateOverride = nil, 0 })
	t.Setenv("FEED_ME_PUBLIC_BASE_URL", "")

	dir := t.TempDir()
	sites, err := filepath.Abs(filepath.Join(root, "sites"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "feed-me.yaml")
	must(t, os.WriteFile(cfg, []byte("public_base_url: https://rss.example.com\nsites_dir: "+sites+
		"\nstore_path: "+filepath.Join(dir, "feed-me.db")+"\nout_dir: "+filepath.Join(dir, "out")+"\n"), 0o644))

	var stdout, stderr bytes.Buffer
	code := run([]string{"build", "--config", cfg, "--site", "claude-blog"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit %d, want 1\nstderr: %s", code, stderr.String())
	}
	feed, err := os.ReadFile(filepath.Join(dir, "out", "feeds", "claude-blog.xml"))
	if err != nil {
		t.Fatalf("feed not written: %v\nstdout: %s", err, stdout.String())
	}
	if n := strings.Count(string(feed), "<item>"); n != 6 {
		t.Errorf("feed has %d items, want the 6 saved posts", n)
	}
	atom, err := os.ReadFile(filepath.Join(dir, "out", "feeds", "claude-blog.atom"))
	if err != nil {
		t.Fatalf("atom feed not written: %v", err)
	}
	if n := strings.Count(string(atom), "<entry>"); n != 6 {
		t.Errorf("atom feed has %d entries, want 6", n)
	}
	if p := rssfeed.CheckAtom(atom); len(p) > 0 {
		t.Errorf("atom feed: %v", p)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
