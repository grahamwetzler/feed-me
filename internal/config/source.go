package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/andybalholm/cascadia"
	"gopkg.in/yaml.v3"
)

// SourceKind names where a field value comes from (§3.1, Extractor).
type SourceKind string

const (
	SourceJSONLD  SourceKind = "jsonld"  // jsonld:Type.field[.field...]
	SourceMeta    SourceKind = "meta"    // meta:<name or property>
	SourceCSS     SourceKind = "css"     // css:<selector>[@attr]
	SourceTime    SourceKind = "time"    // time:<selector>[@attr], attr defaults to datetime
	SourceURL     SourceKind = "url"     // url:<regex>
	SourceListing SourceKind = "listing" // listing:<hint field>
	SourceRequest SourceKind = "request" // the URL that was fetched
)

// ListingFields are the hints discovery can attach to a URL.
var ListingFields = []string{"published", "updated", "title", "description", "author", "category", "lastmod"}

// Source is one entry in a field's ordered source list, written as kind:expr[@attr].
type Source struct {
	Raw  string
	Line int

	Kind SourceKind
	Expr string
	Attr string

	// JSONLD source: the @type to match and the property path below it.
	JSONLDType string
	JSONLDPath []string

	Matcher cascadia.Selector // css, time
	Regexp  *regexp.Regexp    // url
}

func (s *Source) UnmarshalYAML(n *yaml.Node) error { return scalar(n, &s.Raw, &s.Line) }

// attrPattern matches a trailing @attr. Anchoring to a valid attribute name
// keeps an @ elsewhere in a selector from being misread.
var attrPattern = regexp.MustCompile(`@([A-Za-z_:][-A-Za-z0-9_:.]*)$`)

// ParseSource parses a source spec. It is exported for the check command's --source flag.
func ParseSource(raw string) (Source, error) {
	s := Source{Raw: raw}
	return s, s.compile()
}

func (s *Source) compile() error {
	raw := strings.TrimSpace(s.Raw)
	if raw == string(SourceRequest) {
		s.Kind = SourceRequest
		return nil
	}
	kind, expr, ok := strings.Cut(raw, ":")
	if !ok {
		return fmt.Errorf("source %q: want kind:expr (kinds: jsonld, meta, css, time, url, listing, request)", s.Raw)
	}
	s.Kind, s.Expr = SourceKind(kind), strings.TrimSpace(expr)
	if s.Expr == "" {
		return fmt.Errorf("source %q: empty expression", s.Raw)
	}

	switch s.Kind {
	case SourceJSONLD:
		parts := strings.Split(s.Expr, ".")
		for _, p := range parts {
			if p == "" {
				return fmt.Errorf("source %q: empty path segment", s.Raw)
			}
		}
		if len(parts) < 2 {
			return fmt.Errorf("source %q: want jsonld:Type.property, e.g. jsonld:BlogPosting.headline", s.Raw)
		}
		s.JSONLDType, s.JSONLDPath = parts[0], parts[1:]

	case SourceMeta:
		// Meta names contain colons (og:title, article:published_time); nothing to check.

	case SourceCSS, SourceTime:
		sel := s.Expr
		if m := attrPattern.FindStringSubmatchIndex(sel); m != nil {
			s.Attr = sel[m[2]:m[3]]
			sel = strings.TrimSpace(sel[:m[0]])
		}
		if s.Kind == SourceTime && s.Attr == "" {
			s.Attr = "datetime"
		}
		m, err := compileSelector(sel)
		if err != nil {
			return fmt.Errorf("source %q: %v", s.Raw, err)
		}
		s.Expr, s.Matcher = sel, m

	case SourceURL:
		re, err := regexp.Compile(s.Expr)
		if err != nil {
			return fmt.Errorf("source %q: invalid regex: %v", s.Raw, err)
		}
		s.Regexp = re

	case SourceListing:
		if !contains(ListingFields, s.Expr) {
			return fmt.Errorf("source %q: unknown listing field %q (have: %s)", s.Raw, s.Expr, strings.Join(ListingFields, ", "))
		}

	case SourceRequest:
		return fmt.Errorf("source %q: request takes no expression", s.Raw)

	default:
		return fmt.Errorf("source %q: unknown kind %q (kinds: jsonld, meta, css, time, url, listing, request)", s.Raw, kind)
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
