package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSource(t *testing.T) {
	tests := []struct {
		raw               string
		kind              SourceKind
		expr, attr, jtype string
		wantErr           string
	}{
		{raw: "jsonld:BlogPosting.headline", kind: SourceJSONLD, expr: "BlogPosting.headline", jtype: "BlogPosting"},
		{raw: "meta:og:title", kind: SourceMeta, expr: "og:title"},
		{raw: "meta:article:published_time", kind: SourceMeta, expr: "article:published_time"},
		{raw: "css:h1", kind: SourceCSS, expr: "h1"},
		{raw: "css:link[rel=canonical]@href", kind: SourceCSS, expr: "link[rel=canonical]", attr: "href"},
		{raw: "css:article img@src", kind: SourceCSS, expr: "article img", attr: "src"},
		{raw: `css:div[class~="prose-p:!my-0"]`, kind: SourceCSS, expr: `div[class~="prose-p:!my-0"]`},
		{raw: "css:p.uppercase > span:last-child", kind: SourceCSS, expr: "p.uppercase > span:last-child"},
		{raw: "time:article time", kind: SourceTime, expr: "article time", attr: "datetime"},
		{raw: "time:time@data-date", kind: SourceTime, expr: "time", attr: "data-date"},
		{raw: `url:/(\d{4})/(\d{2})/(\d{2})/`, kind: SourceURL, expr: `/(\d{4})/(\d{2})/(\d{2})/`},
		{raw: "listing:published", kind: SourceListing, expr: "published"},
		{raw: "request", kind: SourceRequest},

		{raw: "jsonld:BlogPosting", wantErr: "want jsonld:Type.property"},
		{raw: "jsonld:BlogPosting..x", wantErr: "empty path segment"},
		{raw: "css:div[", wantErr: "invalid CSS selector"},
		{raw: "url:(", wantErr: "invalid regex"},
		{raw: "listing:nope", wantErr: "unknown listing field"},
		{raw: "xpath://h1", wantErr: "unknown kind"},
		{raw: "meta:", wantErr: "empty expression"},
		{raw: "h1", wantErr: "want kind:expr"},
		{raw: "request:x", wantErr: "takes no expression"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			s, err := ParseSource(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.Kind != tt.kind || s.Expr != tt.expr || s.Attr != tt.attr || s.JSONLDType != tt.jtype {
				t.Errorf("got kind=%q expr=%q attr=%q jtype=%q", s.Kind, s.Expr, s.Attr, s.JSONLDType)
			}
		})
	}
}

func TestRate(t *testing.T) {
	for raw, want := range map[string]float64{"1/s": 1, "30/m": 0.5, "1/2s": 0.5, "3600/h": 1, "0.5/s": 0.5} {
		r := Rate{Raw: raw}
		if err := r.compile(); err != nil || r.PerSecond != want {
			t.Errorf("%q: got %v, %v; want %v", raw, r.PerSecond, err, want)
		}
	}
	for _, raw := range []string{"1", "0/s", "-1/s", "1/x", "a/s", "1/0s", "NaN/s", "Inf/s", "+Inf/s", "1e308/1ns", "5e-324/1h"} {
		r := Rate{Raw: raw}
		if err := r.compile(); err == nil {
			t.Errorf("%q: want error", raw)
		}
	}
}

func TestLayout(t *testing.T) {
	for _, ok := range []string{"Jan 2, 2006", "January 2, 2006", "Monday, January 2, 2006", "2006-01-02", "02/01/2006 15:04"} {
		l := Layout{Raw: ok}
		if err := l.compile(); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for bad, msg := range map[string]string{"YYYY-MM-DD": "no date or time", "Jan 2": "no year", "": "no date or time"} {
		l := Layout{Raw: bad}
		if err := l.compile(); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q: err = %v, want containing %q", bad, err, msg)
		}
	}
}

func TestLoadShippedConfigs(t *testing.T) {
	t.Setenv(EnvPublicBaseURL, "")
	cfg, err := Load("../../rss-er.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sites) != 2 {
		t.Fatalf("got %d sites", len(cfg.Sites))
	}
	cb := cfg.Site("claude-blog")
	if cb == nil {
		t.Fatal("claude-blog missing")
	}
	if cb.Schedule.Location.String() != "America/Los_Angeles" || cb.Schedule.RefreshWindow.D.Hours() != 336 {
		t.Errorf("schedule defaults: %+v", cb.Schedule)
	}
	if cb.Fetch.Rate.PerSecond != 1 || !cb.RespectRobots() || !cb.KeepFirstImage() {
		t.Errorf("fetch defaults not inherited: %+v", cb.Fetch)
	}
	if len(cb.Item.Canonical) != 3 || cb.Item.Canonical[2].Kind != SourceRequest {
		t.Errorf("canonical defaults: %+v", cb.Item.Canonical)
	}
	if d := cb.Discovery[0]; !d.Accept("https://claude.com/blog/some-post") || d.Accept("https://claude.com/ja/blog/some-post") {
		t.Error("claude-blog include regex")
	}
	sd := cfg.Site("select-dev")
	if sd.Item.Published.Sources[0].Kind != SourceListing {
		t.Errorf("select-dev published: %+v", sd.Item.Published.Sources)
	}
}

func TestPublicBaseURLFromEnv(t *testing.T) {
	t.Setenv(EnvPublicBaseURL, "https://rss.example.com/")
	g, err := LoadGlobal("../../rss-er.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if g.PublicBaseURL != "https://rss.example.com" {
		t.Errorf("got %q", g.PublicBaseURL)
	}
}

// writeSite writes a site file into a temp sites dir alongside a minimal global config.
func writeSite(t *testing.T, name, body string) (globalPath, sitePath string) {
	t.Helper()
	dir := t.TempDir()
	globalPath = filepath.Join(dir, "rss-er.yaml")
	must(t, os.WriteFile(globalPath, []byte("public_base_url: https://rss.example.com\n"), 0o644))
	must(t, os.Mkdir(filepath.Join(dir, "sites"), 0o755))
	sitePath = filepath.Join(dir, "sites", name)
	must(t, os.WriteFile(sitePath, []byte(body), 0o644))
	return globalPath, sitePath
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

const brokenSite = `id: broken
version: 1
channel:
  title: Broken
  link: example.com
  descripton: typo
schedule:
  interval: soon
discovery:
  - type: sitemap
    url: https://example.com/sitemap.xml
    include: ['[']
    link_selector: a
item:
  title: [cs:h1, listing:published]
  published:
    sources: [meta:date]
    layouts: ['YYYY']
  content:
    selector: 'div:not('
`

func TestLoadReportsAllErrorsWithLines(t *testing.T) {
	g, _ := writeSite(t, "broken.yaml", brokenSite)
	_, err := Load(g)
	var errs Errors
	if !errors.As(err, &errs) {
		t.Fatalf("want Errors, got %v", err)
	}
	want := map[int]string{
		3:  "channel.description: is required",
		5:  "channel.link: must be an absolute",
		6:  `unknown key "descripton"`,
		8:  "schedule.interval: invalid duration",
		12: "discovery[0].include[0]: invalid regex",
		13: "discovery[0].link_selector: only applies to index",
		15: "listing:published",
		18: "item.published.layouts[0]",
		20: "item.content.selector: invalid CSS selector",
	}
	got := map[int][]string{}
	for _, e := range errs {
		if !strings.HasSuffix(e.File, "broken.yaml") {
			t.Errorf("error without site file: %v", e)
		}
		got[e.Line] = append(got[e.Line], e.Error())
	}
	for line, frag := range want {
		found := false
		for _, msg := range got[line] {
			found = found || strings.Contains(msg, frag)
		}
		if !found {
			t.Errorf("line %d: no error containing %q; got %q", line, frag, got[line])
		}
	}
	if !strings.Contains(errs.Error(), `source "cs:h1": unknown kind`) {
		t.Errorf("missing unknown-kind error:\n%s", errs)
	}
}

func TestSiteIDMustMatchFilename(t *testing.T) {
	g, sp := writeSite(t, "other.yaml", `id: mine
version: 1
channel: {title: T, link: https://example.com, description: D}
discovery: [{type: links, urls: [https://example.com/a]}]
item: {title: [css:h1], content: {selector: article}}
`)
	gl, err := LoadGlobal(g)
	must(t, err)
	_, errs := LoadSite(sp, gl)
	if len(errs) != 1 || !strings.Contains(errs[0].Msg, "must be named <id>.yaml") {
		t.Fatalf("got %v", errs)
	}
}

func TestDuplicateAndMissingSites(t *testing.T) {
	g, _ := writeSite(t, "a.yml", "")
	_, err := Load(g)
	if err == nil || !strings.Contains(err.Error(), "file is empty") {
		t.Errorf("empty file: %v", err)
	}

	dir := t.TempDir()
	gp := filepath.Join(dir, "rss-er.yaml")
	must(t, os.WriteFile(gp, []byte("public_base_url: https://x.example\nsites_dir: nope\n"), 0o644))
	if _, err := Load(gp); err == nil || !strings.Contains(err.Error(), "sites_dir") {
		t.Errorf("missing sites dir: %v", err)
	}
}

func TestLineOf(t *testing.T) {
	g, sp := writeSite(t, "broken.yaml", brokenSite)
	gl, err := LoadGlobal(g)
	must(t, err)
	s, _ := LoadSite(sp, gl)
	for _, tt := range []struct {
		path []any
		line int
	}{
		{p("channel", "link"), 5},
		{p("channel", "nope"), 3}, // falls back to the enclosing key
		{p("discovery", 0, "url"), 11},
		{p("item", "title", 1), 15},
		{p("missing"), 0},
	} {
		if got := lineOf(s.root, tt.path...); got != tt.line {
			t.Errorf("%v: got %d, want %d", tt.path, got, tt.line)
		}
	}
}

func TestRejectsMultipleDocuments(t *testing.T) {
	g, sp := writeSite(t, "multi.yaml", `id: multi
version: 1
channel: {title: T, link: https://example.com, description: D}
discovery: [{type: links, urls: [https://example.com/a]}]
item: {title: [css:h1], content: {selector: article}}
---
id: ignored
`)
	gl, err := LoadGlobal(g)
	must(t, err)
	_, errs := LoadSite(sp, gl)
	if len(errs) != 1 || !strings.Contains(errs[0].Msg, "only one YAML document") || errs[0].Line != 6 {
		t.Fatalf("got %v", errs)
	}
}

func TestHeaderValidation(t *testing.T) {
	g, sp := writeSite(t, "hdr.yaml", `id: hdr
version: 1
channel: {title: T, link: https://example.com, description: D}
fetch:
  headers:
    X@Test: a
    X-Block: |
      value
discovery: [{type: links, urls: [https://example.com/a]}]
item: {title: [css:h1], content: {selector: article}}
`)
	gl, err := LoadGlobal(g)
	must(t, err)
	_, errs := LoadSite(sp, gl)
	msgs := errs.Error()
	if len(errs) != 2 || !strings.Contains(msgs, `invalid header name "X@Test"`) || !strings.Contains(msgs, "invalid header value") {
		t.Fatalf("got %v", errs)
	}
}

func TestListenNeedsAPort(t *testing.T) {
	t.Setenv(EnvPublicBaseURL, "")
	for listen, ok := range map[string]bool{":8080": true, "127.0.0.1:9000": true, "[::]:80": true, "9000": false, "localhost": false, "localhost:": false} {
		path := filepath.Join(t.TempDir(), "rss-er.yaml")
		if err := os.WriteFile(path, []byte("public_base_url: https://rss.example.com\nlisten: \""+listen+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadGlobal(path)
		if (err == nil) != ok || (err != nil && !strings.Contains(err.Error(), "listen")) {
			t.Errorf("listen %q: err = %v", listen, err)
		}
	}
}

func TestBasePathMustBeLiteral(t *testing.T) {
	t.Setenv(EnvPublicBaseURL, "")
	for base, want := range map[string]string{
		"/": "/", "/rss": "/rss/", "/rss/": "/rss/", "/a/b-c.d~e_f/": "/a/b-c.d~e_f/",
		"/a{b/": "", "/{x}/": "", "/a//b/": "", "/a b/": "", "/../": "", "/a/./": "", "/%41/": "",
	} {
		path := filepath.Join(t.TempDir(), "rss-er.yaml")
		must(t, os.WriteFile(path, []byte("public_base_url: https://rss.example.com\nbase_path: \""+base+"\"\n"), 0o644))
		g, err := LoadGlobal(path)
		switch {
		case want == "" && (err == nil || !strings.Contains(err.Error(), "base_path")):
			t.Errorf("base_path %q: err = %v, want a base_path error", base, err)
		case want != "" && err != nil:
			t.Errorf("base_path %q: %v", base, err)
		case want != "" && g.BasePath != want:
			t.Errorf("base_path %q became %q, want %q", base, g.BasePath, want)
		}
	}
}

// The container's config must load from where the image and compose put it,
// with ../sites resolving to the shipped sites.
func TestLoadDeployConfig(t *testing.T) {
	t.Setenv(EnvPublicBaseURL, "")
	cfg, err := Load("../../deploy/rss-er.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs("../../sites")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := filepath.Abs(cfg.Global.SitesDir); got != want {
		t.Errorf("sites_dir = %s, want %s", cfg.Global.SitesDir, want)
	}
	if len(cfg.Sites) == 0 || cfg.Global.StorePath != "/data/rss-er.db" {
		t.Errorf("%d sites, store_path %s", len(cfg.Sites), cfg.Global.StorePath)
	}
}
