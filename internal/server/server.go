// Package server serves the rendered feeds over HTTP (§8): the feeds, an
// index page, an OPML export, and health and readiness probes.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"feed-me/internal/config"
	"feed-me/internal/pipeline"
	"feed-me/internal/store"
)

// CacheControl lets a reverse proxy or CDN cache feeds briefly (§8.1).
const CacheControl = "public, max-age=300"

// Server holds the last good rendering of every feed. Handlers only read the
// store (for readiness) and this cache; Refresh replaces entries after a run.
type Server struct {
	global    *config.Global
	sites     []*config.Site
	store     *store.Store
	generator string
	log       *slog.Logger
	formats   []pipeline.Format
	now       func() time.Time // replaceable in tests

	mu    sync.RWMutex
	feeds map[string]*rendered // by path below base_path, e.g. feeds/x.xml
}

type rendered struct {
	data     []byte
	etag     string
	modified time.Time
	ctype    string
}

func New(g *config.Global, sites []*config.Site, st *store.Store, generator string, log *slog.Logger) *Server {
	return &Server{global: g, sites: sites, store: st, generator: generator, log: log,
		formats: pipeline.Formats, now: time.Now, feeds: map[string]*rendered{}}
}

// Refresh re-renders every format of a site's feed from the store. A feed
// that renders empty or fails its checks is not published; the last good one
// keeps being served (§8.1, observability).
//
// Last-Modified moves whenever the bytes do, not only when items change: a
// config edit or a new version can change a feed without a new item. It
// survives a restart that renders the same bytes.
func (s *Server) Refresh(ctx context.Context, site *config.Site) error {
	var errs []string
	for _, f := range s.formats {
		path := pipeline.FeedPath(site.ID, f)
		data, n, err := pipeline.Render(ctx, s.store, s.global, site, s.generator, f)
		switch {
		case err != nil:
			errs = append(errs, fmt.Sprintf("%s: %v", path, err))
			continue
		case n == 0:
			errs = append(errs, path+": no items stored")
			continue
		}
		if probs := f.Check(data); len(probs) > 0 {
			errs = append(errs, fmt.Sprintf("%s: failed its checks: %s", path, strings.Join(probs, "; ")))
			continue
		}
		sum := sha256.Sum256(data)
		r := &rendered{data: data, etag: `"` + hex.EncodeToString(sum[:16]) + `"`, ctype: f.ContentType}
		r.modified = s.modified(ctx, path, r.etag)
		s.mu.Lock()
		s.feeds[path] = r
		s.mu.Unlock()
	}
	if len(errs) > 0 {
		return fmt.Errorf("keeping the last good feed: %s", strings.Join(errs, "; "))
	}
	return nil
}

// modified is when the feed at path last changed: unchanged if its ETag
// matches the one served or, after a restart, the one stored; otherwise now,
// which is stored for the next restart.
func (s *Server) modified(ctx context.Context, path, etag string) time.Time {
	s.mu.RLock()
	prev := s.feeds[path]
	s.mu.RUnlock()
	if prev != nil && prev.etag == etag {
		return prev.modified
	}
	if prev == nil {
		stamp, err := s.store.FeedStamp(ctx, path)
		if err != nil {
			s.log.Warn("reading feed stamp", "feed", path, "err", err)
		} else if stamp.ETag == etag {
			return stamp.Modified
		}
	}
	now := s.now().UTC()
	if err := s.store.PutFeedStamp(ctx, path, store.FeedStamp{ETag: etag, Modified: now}); err != nil {
		s.log.Warn("storing feed stamp; Last-Modified will move on restart", "feed", path, "err", err)
	}
	return now
}

// Handler serves everything below base_path; any other path is a 404.
func (s *Server) Handler() http.Handler {
	base := s.global.BasePath // starts and ends with /
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base+"feeds/{file}", s.serveFeed)
	mux.HandleFunc("GET "+base+"feeds.opml", s.serveOPML)
	mux.HandleFunc("GET "+base+"healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET "+base+"readyz", s.serveReady)
	mux.HandleFunc("GET "+base+"{$}", s.serveIndex)
	if base != "/" {
		// The bare prefix goes to the index, staying inside base_path.
		mux.Handle("GET "+strings.TrimSuffix(base, "/"), http.RedirectHandler(base, http.StatusMovedPermanently))
	}
	return mux
}

func (s *Server) serveFeed(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	f := s.feeds["feeds/"+r.PathValue("file")]
	s.mu.RUnlock()
	if f == nil {
		if s.known(r.PathValue("file")) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "feed not built yet", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("ETag", f.etag)
	h.Set("Cache-Control", CacheControl)
	// ServeContent answers conditional requests with 304 and handles HEAD.
	http.ServeContent(w, r, "", f.modified, bytes.NewReader(f.data))
}

// known reports whether file names a configured site's feed.
func (s *Server) known(file string) bool {
	for _, site := range s.sites {
		for _, f := range s.formats {
			if file == site.ID+f.Ext {
				return true
			}
		}
	}
	return false
}

// serveReady answers 200 once every site has completed a successful run
// (§8.1) and every one of its feeds can be served. One unservable site keeps
// the whole process not ready on purpose, to surface it; the other feeds are
// still served, and traffic and restarts go by /healthz.
func (s *Server) serveReady(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var waiting []string
	for _, site := range s.sites {
		st, err := s.store.SiteState(r.Context(), site.ID)
		if err != nil {
			s.log.Error("readyz: reading site state", "site", site.ID, "err", err)
			http.Error(w, "store unavailable", http.StatusServiceUnavailable)
			return
		}
		if st.LastSuccess.IsZero() || !s.served(site.ID) {
			waiting = append(waiting, site.ID)
		}
	}
	if len(waiting) > 0 {
		http.Error(w, "waiting for a successful run and feed: "+strings.Join(waiting, ", "), http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ready")
}

// served reports whether every format of a site's feed is cached.
func (s *Server) served(siteID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, f := range s.formats {
		if s.feeds[pipeline.FeedPath(siteID, f)] == nil {
			return false
		}
	}
	return true
}

// FeedURL is the public URL of a site's feed.
func (s *Server) FeedURL(siteID string, f pipeline.Format) string {
	return s.global.PublicBaseURL + s.global.BasePath + pipeline.FeedPath(siteID, f)
}

type opml struct {
	XMLName xml.Name      `xml:"opml"`
	Version string        `xml:"version,attr"`
	Title   string        `xml:"head>title"`
	Outline []opmlOutline `xml:"body>outline"`
}

type opmlOutline struct {
	Type    string `xml:"type,attr"`
	Text    string `xml:"text,attr"`
	Title   string `xml:"title,attr"`
	XMLURL  string `xml:"xmlUrl,attr"`
	HTMLURL string `xml:"htmlUrl,attr"`
}

func (s *Server) serveOPML(w http.ResponseWriter, r *http.Request) {
	doc := opml{Version: "2.0", Title: "Feed Me! feeds", Outline: make([]opmlOutline, len(s.sites))}
	for i, site := range s.sites {
		o := &doc.Outline[i]
		o.Type, o.Text, o.Title = "rss", site.Channel.Title, site.Channel.Title
		o.XMLURL, o.HTMLURL = s.FeedURL(site.ID, pipeline.RSS), site.Channel.Link
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	buf.WriteByte('\n')
	w.Header().Set("Content-Type", "text/x-opml; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="feed-me.opml"`)
	w.Write(buf.Bytes())
}

var index = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Feed Me!</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:2rem auto;padding:0 1rem}li{margin:.5rem 0}</style>
<h1>Feed Me! feeds</h1>
<ul>
{{- range .Sites}}
<li><a href="{{.Link}}">{{.Title}}</a>: <a href="{{.RSS}}">RSS</a> · <a href="{{.Atom}}">Atom</a></li>
{{- end}}
</ul>
<p><a href="{{.OPML}}">OPML</a> for importing every feed at once.</p>
`))

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	type entry struct{ Title, Link, RSS, Atom string }
	data := struct {
		Sites []entry
		OPML  string
	}{OPML: s.global.PublicBaseURL + s.global.BasePath + "feeds.opml"}
	for _, site := range s.sites {
		data.Sites = append(data.Sites, entry{site.Channel.Title, site.Channel.Link,
			s.FeedURL(site.ID, pipeline.RSS), s.FeedURL(site.ID, pipeline.Atom)})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := index.Execute(w, data); err != nil {
		s.log.Error("rendering index", "err", err)
	}
}
