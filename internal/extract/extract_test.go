package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rss-er/internal/config"
)

// site loads a minimal site config whose item section is itemYAML.
func site(t *testing.T, itemYAML string) *config.Site {
	t.Helper()
	dir := t.TempDir()
	g := filepath.Join(dir, "rss-er.yaml")
	if err := os.WriteFile(g, []byte("public_base_url: https://rss.example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sites"), 0o755); err != nil {
		t.Fatal(err)
	}
	sp := filepath.Join(dir, "sites", "s.yaml")
	body := "id: s\nversion: 1\nchannel: {title: T, link: https://example.com, description: D}\n" +
		"discovery: [{type: links, urls: [https://example.com/a]}]\nitem:\n" + itemYAML
	if err := os.WriteFile(sp, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gl, err := config.LoadGlobal(g)
	if err != nil {
		t.Fatal(err)
	}
	s, errs := config.LoadSite(sp, gl)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	return s
}

func TestJSONLDNestedObjectsResolveDeterministically(t *testing.T) {
	s := site(t, "  title: [jsonld:Thing.name]\n  content: {selector: article}\n")
	page := []byte(`<script type="application/ld+json">{"@type":"Page","zeta":{"@type":"Thing","name":"Z"},"alpha":{"@type":"Thing","name":"A"},"mid":[{"@type":"Thing","name":"M"}]}</script><article>x</article>`)
	for i := 0; i < 50; i++ {
		r, err := Extract(s, "https://example.com/a", page, nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.Title != "A" {
			t.Fatalf("run %d: title = %q, want A (first key in sorted order)", i, r.Title)
		}
	}
}

func TestJSONLDDateFallsThroughToLaterObject(t *testing.T) {
	s := site(t, "  title: [css:h1]\n  published: {sources: [jsonld:BlogPosting.datePublished]}\n  content: {selector: article}\n")
	page := []byte(`<script type="application/ld+json">{"@type":"BlogPosting","datePublished":"soon"}</script>
<script type="application/ld+json">{"@type":"BlogPosting","datePublished":"2026-09-21T15:12:00Z"}</script><h1>T</h1><article>x</article>`)
	r, err := Extract(s, "https://example.com/a", page, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Published.IsZero() || r.Published.UTC().Format("2006-01-02T15:04") != "2026-09-21T15:12" {
		t.Errorf("published = %v, warnings %q", r.Published, r.Warnings)
	}
}

func TestExcludedSubtreeThatAlsoMatchesIsNotKept(t *testing.T) {
	s := site(t, "  title: [css:h1]\n  content: {selector: .body, exclude: [.ad]}\n")
	page := []byte(`<h1>T</h1><div class="body"><p>keep</p><div class="body ad">buy now</div></div>`)
	r, err := Extract(s, "https://example.com/a", page, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Content, "buy now") || !strings.Contains(r.Content, "keep") || r.ContentMatches != 1 {
		t.Errorf("content = %q (%d matches)", r.Content, r.ContentMatches)
	}
}
