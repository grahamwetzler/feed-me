package config

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/cascadia"
	"gopkg.in/yaml.v3"
)

// The scalar types below capture the raw YAML value and its position at decode
// time, and are compiled during validation. Decoding never fails on a bad
// value, so validation can report every problem in a file at once.

// scalar is the shared decode step: accept any scalar node, remember where it was.
func scalar(n *yaml.Node, raw *string, line *int) error {
	if n.Kind != yaml.ScalarNode {
		return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: expected a string, got a %s", n.Line, kindName(n.Kind))}}
	}
	*raw, *line = n.Value, n.Line
	return nil
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.MappingNode:
		return "mapping"
	case yaml.SequenceNode:
		return "list"
	case yaml.AliasNode:
		return "alias"
	default:
		return "scalar"
	}
}

// Duration is a Go duration string such as "1h" or "336h".
type Duration struct {
	Raw  string
	Line int
	D    time.Duration
}

func (d *Duration) UnmarshalYAML(n *yaml.Node) error { return scalar(n, &d.Raw, &d.Line) }

func (d *Duration) compile() error {
	v, err := time.ParseDuration(d.Raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q (use Go syntax such as 30s, 1h, 336h)", d.Raw)
	}
	if v <= 0 {
		return fmt.Errorf("duration %q must be positive", d.Raw)
	}
	d.D = v
	return nil
}

// Rate is a request rate such as "1/s", "30/m" or "1/2s" (one request every two seconds).
type Rate struct {
	Raw       string
	Line      int
	PerSecond float64
}

func (r *Rate) UnmarshalYAML(n *yaml.Node) error { return scalar(n, &r.Raw, &r.Line) }

func (r *Rate) compile() error {
	num, per, ok := strings.Cut(r.Raw, "/")
	if !ok {
		return fmt.Errorf("invalid rate %q (want N/unit, e.g. 1/s, 30/m, 1/2s)", r.Raw)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil || !(n > 0) || math.IsInf(n, 0) {
		return fmt.Errorf("invalid rate %q: count must be a positive number", r.Raw)
	}
	per = strings.TrimSpace(per)
	switch per {
	case "s", "m", "h":
		per = "1" + per
	}
	d, err := time.ParseDuration(per)
	if err != nil || d <= 0 {
		return fmt.Errorf("invalid rate %q: unit must be s, m, h or a duration", r.Raw)
	}
	r.PerSecond = n / d.Seconds()
	if !(r.PerSecond > 0) || math.IsInf(r.PerSecond, 0) {
		return fmt.Errorf("invalid rate %q: out of range", r.Raw)
	}
	return nil
}

// Regex is a Go regular expression.
type Regex struct {
	Raw  string
	Line int
	Re   *regexp.Regexp
}

func (r *Regex) UnmarshalYAML(n *yaml.Node) error { return scalar(n, &r.Raw, &r.Line) }

func (r *Regex) compile() error {
	re, err := regexp.Compile(r.Raw)
	if err != nil {
		return fmt.Errorf("invalid regex %q: %v", r.Raw, err)
	}
	r.Re = re
	return nil
}

// Selector is a CSS selector (group), compiled with cascadia.
type Selector struct {
	Raw     string
	Line    int
	Matcher cascadia.Selector
}

func (s *Selector) UnmarshalYAML(n *yaml.Node) error { return scalar(n, &s.Raw, &s.Line) }

func (s *Selector) compile() error {
	m, err := compileSelector(s.Raw)
	if err != nil {
		return err
	}
	s.Matcher = m
	return nil
}

func compileSelector(raw string) (cascadia.Selector, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("empty CSS selector")
	}
	g, err := cascadia.Compile(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid CSS selector %q: %v", raw, err)
	}
	return g, nil
}

// Layout is a Go time layout such as "Jan 2, 2006".
type Layout struct {
	Raw  string
	Line int
}

func (l *Layout) UnmarshalYAML(n *yaml.Node) error { return scalar(n, &l.Raw, &l.Line) }

// layoutRef is a reference time whose every component differs from the layout
// tokens' own digits, so a layout with no tokens can be told apart.
var layoutRef = time.Date(2031, time.November, 28, 19, 47, 33, 0, time.UTC)

func (l *Layout) compile() error {
	s := layoutRef.Format(l.Raw)
	if s == l.Raw {
		return fmt.Errorf("time layout %q contains no date or time elements (layouts use Go's reference time: Mon Jan 2 15:04:05 MST 2006)", l.Raw)
	}
	t, err := time.Parse(l.Raw, s)
	if err != nil {
		return fmt.Errorf("time layout %q does not round-trip: %v", l.Raw, err)
	}
	if t.Format(l.Raw) != s {
		return fmt.Errorf("time layout %q does not round-trip (%q parsed back as %q)", l.Raw, s, t.Format(l.Raw))
	}
	if t.Year() != layoutRef.Year() {
		return fmt.Errorf("time layout %q has no year (2006)", l.Raw)
	}
	return nil
}
