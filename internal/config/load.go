package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
	"gopkg.in/yaml.v3"
)

// Config is the fully loaded and validated configuration.
type Config struct {
	Global Global
	Sites  []*Site
}

// Site returns the site with the given id, or nil.
func (c *Config) Site(id string) *Site {
	for _, s := range c.Sites {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// Error is one validation problem, positioned at file:line when the line is known.
type Error struct {
	File string
	Line int
	Path string // dotted key path, e.g. item.published.layouts[0]
	Msg  string
}

func (e Error) Error() string {
	var b strings.Builder
	b.WriteString(e.File)
	if e.Line > 0 {
		fmt.Fprintf(&b, ":%d", e.Line)
	}
	b.WriteString(": ")
	if e.Path != "" {
		b.WriteString(e.Path + ": ")
	}
	b.WriteString(e.Msg)
	return b.String()
}

// Errors collects every problem found while loading.
type Errors []Error

func (es Errors) Error() string {
	lines := make([]string, len(es))
	for i, e := range es {
		lines[i] = e.Error()
	}
	return strings.Join(lines, "\n")
}

// Load reads the global config at path and every site file in its sites_dir.
// On failure the error is an Errors listing every problem found.
func Load(path string) (*Config, error) {
	g, errs := loadGlobal(path)
	if len(errs) > 0 {
		return nil, errs
	}
	files, err := siteFiles(g.SitesDir)
	if err != nil {
		return nil, Errors{{File: path, Msg: err.Error()}}
	}
	if len(files) == 0 {
		return nil, Errors{{File: path, Msg: fmt.Sprintf("no site configs (*.yaml) found in %s", g.SitesDir)}}
	}
	cfg := &Config{Global: *g}
	seen := map[string]string{}
	for _, f := range files {
		s, serrs := LoadSite(f, g)
		errs = append(errs, serrs...)
		if s == nil {
			continue
		}
		if prev, dup := seen[s.ID]; dup && s.ID != "" {
			errs = append(errs, Error{File: f, Line: lineOf(s.root, "id"), Path: "id", Msg: fmt.Sprintf("duplicate site id %q (also in %s)", s.ID, prev)})
		}
		seen[s.ID] = f
		cfg.Sites = append(cfg.Sites, s)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return cfg, nil
}

func siteFiles(dir string) ([]string, error) {
	var files []string
	for _, pat := range []string{"*.yaml", "*.yml"} {
		m, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil {
			return nil, err
		}
		files = append(files, m...)
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("sites_dir: %v", err)
	}
	sort.Strings(files)
	return files, nil
}

// decodeStrict decodes a YAML file into v, rejecting unknown keys, and also
// returns the node tree so validation can report line numbers.
func decodeStrict(path string, v any) (*yaml.Node, Errors) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, Errors{{File: path, Msg: err.Error()}}
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, yamlErrors(path, err)
	}
	if len(root.Content) == 0 {
		return nil, Errors{{File: path, Msg: "file is empty"}}
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		// A TypeError (unknown key, wrong type) still decodes everything else,
		// so validation can go on and report the rest of the file's problems.
		var te *yaml.TypeError
		if errors.As(err, &te) {
			return &root, yamlErrors(path, err)
		}
		return nil, yamlErrors(path, err)
	}
	// A second document would otherwise be silently ignored.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, Errors{{File: path, Line: extra.Line, Msg: "only one YAML document is allowed (found another after ---)"}}
	} else if !errors.Is(err, io.EOF) {
		return nil, yamlErrors(path, err)
	}
	return &root, nil
}

var unknownField = regexp.MustCompile(`^field (\S+) not found in type \S+$`)

var yamlLinePrefix = regexp.MustCompile(`^(?:yaml: )?line (\d+): `)

// yamlErrors splits a yaml.v3 error into positioned Errors.
func yamlErrors(path string, err error) Errors {
	var msgs []string
	var te *yaml.TypeError
	if errors.As(err, &te) {
		msgs = te.Errors
	} else {
		msgs = []string{err.Error()}
	}
	var out Errors
	for _, m := range msgs {
		e := Error{File: path, Msg: m}
		if sub := yamlLinePrefix.FindStringSubmatch(m); sub != nil {
			e.Line, _ = strconv.Atoi(sub[1])
			e.Msg = m[len(sub[0]):]
		}
		if sub := unknownField.FindStringSubmatch(e.Msg); sub != nil {
			e.Msg = fmt.Sprintf("unknown key %q", sub[1])
		}
		out = append(out, e)
	}
	return out
}

// lineOf finds the line of a key path (string keys, int list indices) in a
// YAML tree. If the full path is absent, it returns the deepest part that
// exists, so "missing key" errors point at the enclosing block.
func lineOf(root *yaml.Node, path ...any) int {
	if root == nil {
		return 0
	}
	n := root
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	line := 0
	for _, p := range path {
		switch k := p.(type) {
		case string:
			if n.Kind != yaml.MappingNode {
				return line
			}
			found := false
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == k {
					line, n, found = n.Content[i].Line, n.Content[i+1], true
					break
				}
			}
			if !found {
				return line
			}
		case int:
			if n.Kind != yaml.SequenceNode || k >= len(n.Content) {
				return line
			}
			n = n.Content[k]
			line = n.Line
		}
	}
	return line
}

// validator accumulates errors for one file.
type validator struct {
	file string
	root *yaml.Node
	errs Errors
}

// errAt records an error at an explicit line, falling back to the path's line.
func (v *validator) errAt(line int, path []any, format string, args ...any) {
	if line == 0 {
		line = lineOf(v.root, path...)
	}
	v.errs = append(v.errs, Error{File: v.file, Line: line, Path: pathString(path), Msg: fmt.Sprintf(format, args...)})
}

func (v *validator) err(path []any, format string, args ...any) { v.errAt(0, path, format, args...) }

func p(parts ...any) []any { return parts }

func pathString(path []any) string {
	var b strings.Builder
	for _, part := range path {
		switch k := part.(type) {
		case string:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(k)
		case int:
			fmt.Fprintf(&b, "[%d]", k)
		}
	}
	return b.String()
}

// absURL validates an absolute http(s) URL.
func (v *validator) absURL(path []any, raw string, required bool) {
	if raw == "" {
		if required {
			v.err(path, "is required")
		}
		return
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		v.err(path, "must be an absolute http(s) URL, got %q", raw)
	}
}

func (v *validator) required(path []any, val string) {
	if strings.TrimSpace(val) == "" {
		v.err(path, "is required")
	}
}

func loadGlobal(path string) (*Global, Errors) {
	g := &Global{File: path}
	root, errs := decodeStrict(path, g)
	if root == nil {
		return nil, errs
	}
	g.root = root
	v := &validator{file: path, root: root, errs: errs}
	dir := filepath.Dir(path)

	g.SitesDir = resolve(dir, orDefault(g.SitesDir, DefaultSitesDir))
	g.StorePath = resolve(dir, orDefault(g.StorePath, DefaultStorePath))
	g.OutDir = resolve(dir, orDefault(g.OutDir, DefaultOutDir))
	g.Listen = orDefault(g.Listen, DefaultListen)
	if _, port, err := net.SplitHostPort(g.Listen); err != nil || port == "" {
		v.err(p("listen"), "must be host:port or :port, got %q", g.Listen)
	}

	if env := os.Getenv(EnvPublicBaseURL); env != "" {
		g.PublicBaseURL = env
	}
	g.PublicBaseURL = strings.TrimRight(g.PublicBaseURL, "/")
	if g.PublicBaseURL == "" {
		v.err(p("public_base_url"), "is required (or set %s); it is used for the feeds' rel=self links", EnvPublicBaseURL)
	} else {
		v.absURL(p("public_base_url"), g.PublicBaseURL, true)
	}

	g.BasePath = orDefault(g.BasePath, DefaultBasePath)
	if !strings.HasPrefix(g.BasePath, "/") {
		v.err(p("base_path"), "must start with /, got %q", g.BasePath)
	}
	if !strings.HasSuffix(g.BasePath, "/") {
		g.BasePath += "/"
	}
	if strings.HasPrefix(g.BasePath, "/") && (!basePath.MatchString(g.BasePath) ||
		strings.Contains(g.BasePath, "/./") || strings.Contains(g.BasePath, "/../")) {
		// It becomes part of the server's route patterns, so it must be literal.
		v.err(p("base_path"), "must be /, or /-separated path segments of letters, digits and -._~, got %q", g.BasePath)
	}
	if g.ContactURL != "" {
		v.absURL(p("contact_url"), g.ContactURL, false)
	}

	g.Log.Format = orDefault(g.Log.Format, "json")
	if g.Log.Format != "json" && g.Log.Format != "text" {
		v.err(p("log", "format"), "must be json or text, got %q", g.Log.Format)
	}
	g.Log.Level = orDefault(g.Log.Level, "info")
	if !contains([]string{"debug", "info", "warn", "error"}, g.Log.Level) {
		v.err(p("log", "level"), "must be debug, info, warn or error, got %q", g.Log.Level)
	}

	if g.Fetch.Rate.Raw == "" {
		g.Fetch.Rate.Raw = DefaultRate
	}
	if err := g.Fetch.Rate.compile(); err != nil {
		v.errAt(g.Fetch.Rate.Line, p("fetch", "rate"), "%v", err)
	}
	if g.Fetch.Timeout.Raw == "" {
		g.Fetch.Timeout.Raw = DefaultTimeout
	}
	if err := g.Fetch.Timeout.compile(); err != nil {
		v.errAt(g.Fetch.Timeout.Line, p("fetch", "timeout"), "%v", err)
	}
	if g.Fetch.MaxBodyBytes == 0 {
		g.Fetch.MaxBodyBytes = DefaultMaxBodyBytes
	} else if g.Fetch.MaxBodyBytes < 0 {
		v.err(p("fetch", "max_body_bytes"), "must be positive")
	}
	return g, v.errs
}

var siteIDPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// basePath is a base_path the server can put into its route patterns as is.
// Dot segments are rejected separately, since the router cleans them away.
var basePath = regexp.MustCompile(`^/([A-Za-z0-9._~-]+/)*$`)

// LoadSite reads and validates one site file. Fetch settings it leaves unset
// are inherited from g.
func LoadSite(path string, g *Global) (*Site, Errors) {
	s := &Site{File: path}
	root, errs := decodeStrict(path, s)
	if root == nil {
		return nil, errs
	}
	s.root = root
	v := &validator{file: path, root: root, errs: errs}

	// Identity.
	switch {
	case s.ID == "":
		v.err(p("id"), "is required")
	case !siteIDPattern.MatchString(s.ID):
		v.err(p("id"), "must match [a-z0-9-]+, got %q", s.ID)
	default:
		base := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".yaml"), ".yml")
		if base != s.ID {
			v.err(p("id"), "is %q but the file is named %s; the file must be named <id>.yaml", s.ID, filepath.Base(path))
		}
	}
	switch s.Version {
	case 0:
		v.err(p("version"), "is required (current schema version is 1)")
	case 1:
	default:
		v.err(p("version"), "unsupported schema version %d (this build supports 1)", s.Version)
	}

	// Channel.
	v.required(p("channel", "title"), s.Channel.Title)
	v.absURL(p("channel", "link"), s.Channel.Link, true)
	v.required(p("channel", "description"), s.Channel.Description)
	v.absURL(p("channel", "image"), s.Channel.Image, false)
	if s.Channel.TTL < 0 {
		v.err(p("channel", "ttl"), "must not be negative")
	}

	// Schedule.
	durationOr(v, &s.Schedule.Interval, DefaultInterval, p("schedule", "interval"))
	durationOr(v, &s.Schedule.RefreshWindow, DefaultRefreshWindow, p("schedule", "refresh_window"))
	if s.Schedule.MaxItems == 0 {
		s.Schedule.MaxItems = DefaultMaxItems
	} else if s.Schedule.MaxItems < 0 {
		v.err(p("schedule", "max_items"), "must be positive")
	}
	s.Schedule.Timezone = orDefault(s.Schedule.Timezone, DefaultTimezone)
	if loc, err := time.LoadLocation(s.Schedule.Timezone); err != nil {
		v.err(p("schedule", "timezone"), "unknown timezone %q (use an IANA name such as America/Los_Angeles)", s.Schedule.Timezone)
	} else {
		s.Schedule.Location = loc
	}

	// Fetch, inheriting global defaults.
	if s.Fetch.Rate.Raw == "" {
		s.Fetch.Rate = g.Fetch.Rate
	} else if err := s.Fetch.Rate.compile(); err != nil {
		v.errAt(s.Fetch.Rate.Line, p("fetch", "rate"), "%v", err)
	}
	if s.Fetch.Timeout.Raw == "" {
		s.Fetch.Timeout = g.Fetch.Timeout
	} else if err := s.Fetch.Timeout.compile(); err != nil {
		v.errAt(s.Fetch.Timeout.Line, p("fetch", "timeout"), "%v", err)
	}
	for k, val := range s.Fetch.Headers {
		if !httpguts.ValidHeaderFieldName(k) {
			v.err(p("fetch", "headers"), "invalid header name %q", k)
		} else if !httpguts.ValidHeaderFieldValue(val) {
			v.err(p("fetch", "headers", k), "invalid header value %q (no control characters or newlines)", val)
		}
	}
	for i, h := range s.Fetch.HeaderHosts {
		if u, err := url.Parse("//" + h); err != nil || h == "" || u.Host != h || u.Port() != "" && u.Hostname() == "" {
			v.err(p("fetch", "header_hosts", i), "must be a host or host:port, like www.example.com, got %q", h)
		}
	}

	// Discovery.
	if len(s.Discovery) == 0 {
		v.err(p("discovery"), "at least one discovery strategy is required")
	}
	hints := map[string]bool{} // listing fields some discovery strategy can supply
	for i := range s.Discovery {
		validateDiscovery(v, &s.Discovery[i], i, hints)
	}

	// Item.
	it := &s.Item
	if len(it.Canonical) == 0 {
		for _, raw := range DefaultCanonical {
			it.Canonical = append(it.Canonical, Source{Raw: raw})
		}
	}
	if len(it.Title) == 0 {
		v.err(p("item", "title"), "at least one source is required")
	}
	for _, f := range []struct {
		name string
		srcs []Source
	}{
		{"canonical", it.Canonical}, {"title", it.Title}, {"summary", it.Summary},
		{"author", it.Author}, {"image", it.Image}, {"categories", it.Categories},
	} {
		validateSources(v, f.srcs, p("item", f.name), hints)
	}
	validateDate(v, &it.Published, "published", hints)
	validateDate(v, &it.Updated, "updated", hints)

	if it.Content.Selector.Raw == "" {
		v.err(p("item", "content", "selector"), "is required")
	} else if err := it.Content.Selector.compile(); err != nil {
		v.errAt(it.Content.Selector.Line, p("item", "content", "selector"), "%v", err)
	}
	for i := range it.Content.Exclude {
		if err := it.Content.Exclude[i].compile(); err != nil {
			v.errAt(it.Content.Exclude[i].Line, p("item", "content", "exclude", i), "%v", err)
		}
	}
	it.GUID = orDefault(it.GUID, GUIDPermalink)
	if it.GUID != GUIDPermalink && it.GUID != GUIDStableHash {
		v.err(p("item", "guid"), "must be %s or %s, got %q", GUIDPermalink, GUIDStableHash, it.GUID)
	}

	sort.SliceStable(v.errs, func(i, j int) bool { return v.errs[i].Line < v.errs[j].Line })
	return s, v.errs
}

// listingHints says which listing:<field> hints each discovery type supplies.
var listingHints = map[string][]string{
	DiscoveryFeed:    {"published", "updated", "title", "description", "author", "category"},
	DiscoverySitemap: {"lastmod"},
	DiscoveryIndex:   {"title"},
	DiscoveryLinks:   nil,
}

func validateDiscovery(v *validator, d *Discovery, i int, hints map[string]bool) {
	at := func(k ...any) []any { return append(p("discovery", i), k...) }
	fields, known := listingHints[d.Type]
	if !known {
		v.err(at("type"), "must be one of sitemap, index, feed, links, got %q", d.Type)
		return
	}
	for _, h := range fields {
		hints[h] = true
	}

	// Reject settings that belong to another strategy; they are almost always typos.
	notFor := func(set bool, key, only string) {
		if set {
			v.err(at(key), "only applies to %s discovery", only)
		}
	}
	notFor(d.TrustLastmod && d.Type != DiscoverySitemap, "trust_lastmod", "sitemap")
	notFor(d.LinkSelector.Raw != "" && d.Type != DiscoveryIndex, "link_selector", "index")
	notFor(d.NextSelector.Raw != "" && d.Type != DiscoveryIndex, "next_selector", "index")
	notFor(d.MaxPages != 0 && d.Type != DiscoveryIndex, "max_pages", "index")
	notFor(len(d.URLs) > 0 && d.Type != DiscoveryLinks, "urls", "links")

	if d.Type == DiscoveryLinks {
		if d.URL != "" {
			v.err(at("url"), "links discovery takes urls: [...], not url")
		}
		if len(d.URLs) == 0 {
			v.err(at("urls"), "at least one URL is required")
		}
		for j, u := range d.URLs {
			v.absURL(at("urls", j), u, true)
		}
	} else {
		v.absURL(at("url"), d.URL, true)
	}

	if d.Type == DiscoveryIndex {
		if d.LinkSelector.Raw == "" {
			v.err(at("link_selector"), "is required for index discovery")
		} else if err := d.LinkSelector.compile(); err != nil {
			v.errAt(d.LinkSelector.Line, at("link_selector"), "%v", err)
		}
		if d.NextSelector.Raw != "" {
			if err := d.NextSelector.compile(); err != nil {
				v.errAt(d.NextSelector.Line, at("next_selector"), "%v", err)
			}
		}
		if d.MaxPages == 0 {
			d.MaxPages = 5
		} else if d.MaxPages < 0 {
			v.err(at("max_pages"), "must be positive")
		}
	}

	for j := range d.Include {
		if err := d.Include[j].compile(); err != nil {
			v.errAt(d.Include[j].Line, at("include", j), "%v", err)
		}
	}
	for j := range d.Exclude {
		if err := d.Exclude[j].compile(); err != nil {
			v.errAt(d.Exclude[j].Line, at("exclude", j), "%v", err)
		}
	}
}

func validateSources(v *validator, srcs []Source, path []any, hints map[string]bool) {
	for i := range srcs {
		s := &srcs[i]
		ip := append(append([]any{}, path...), i)
		if err := s.compile(); err != nil {
			v.errAt(s.Line, ip, "%v", err)
			continue
		}
		if s.Kind == SourceListing && !hints[s.Expr] {
			v.errAt(s.Line, ip, "no discovery strategy supplies listing:%s (feed supplies published, updated, title, description, author, category; sitemap supplies lastmod; index supplies title)", s.Expr)
		}
	}
}

func validateDate(v *validator, d *DateField, name string, hints map[string]bool) {
	validateSources(v, d.Sources, p("item", name, "sources"), hints)
	if len(d.Layouts) > 0 && len(d.Sources) == 0 {
		v.err(p("item", name, "layouts"), "layouts are set but there are no sources to parse")
	}
	for i := range d.Layouts {
		if err := d.Layouts[i].compile(); err != nil {
			v.errAt(d.Layouts[i].Line, p("item", name, "layouts", i), "%v", err)
		}
	}
}

func durationOr(v *validator, d *Duration, def time.Duration, path []any) {
	if d.Raw == "" {
		d.D = def
		return
	}
	if err := d.compile(); err != nil {
		v.errAt(d.Line, path, "%v", err)
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func resolve(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}

// LoadGlobal reads and validates only the global config file.
func LoadGlobal(path string) (*Global, error) {
	g, errs := loadGlobal(path)
	if len(errs) > 0 {
		return nil, errs
	}
	return g, nil
}
