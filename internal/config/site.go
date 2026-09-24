package config

import (
	"time"

	"gopkg.in/yaml.v3"
)

// Site is one sites/<id>.yaml file (§4.2).
type Site struct {
	ID        string      `yaml:"id"`
	Version   int         `yaml:"version"`
	Channel   Channel     `yaml:"channel"`
	Schedule  Schedule    `yaml:"schedule"`
	Fetch     Fetch       `yaml:"fetch"`
	Discovery []Discovery `yaml:"discovery"`
	Item      Item        `yaml:"item"`

	File string     `yaml:"-"`
	root *yaml.Node // for looking up line numbers of missing or invalid keys
}

type Channel struct {
	Title       string `yaml:"title"`
	Link        string `yaml:"link"`
	Description string `yaml:"description"`
	Language    string `yaml:"language"`
	Image       string `yaml:"image"`
	Author      string `yaml:"author"`
	TTL         int    `yaml:"ttl"` // minutes
}

type Schedule struct {
	Interval      Duration `yaml:"interval"`
	RefreshWindow Duration `yaml:"refresh_window"`
	MaxItems      int      `yaml:"max_items"`
	Timezone      string   `yaml:"timezone"`

	Location *time.Location `yaml:"-"`
}

type Fetch struct {
	Rate          Rate              `yaml:"rate"`
	Timeout       Duration          `yaml:"timeout"`
	Headers       map[string]string `yaml:"headers"`
	RespectRobots *bool             `yaml:"respect_robots"`
}

// Discovery types (§3.1).
const (
	DiscoverySitemap = "sitemap"
	DiscoveryIndex   = "index"
	DiscoveryFeed    = "feed"
	DiscoveryLinks   = "links"
)

type Discovery struct {
	Type    string  `yaml:"type"`
	URL     string  `yaml:"url"`
	Include []Regex `yaml:"include"`
	Exclude []Regex `yaml:"exclude"`

	// sitemap only
	TrustLastmod bool `yaml:"trust_lastmod"`

	// index only
	LinkSelector Selector `yaml:"link_selector"`
	NextSelector Selector `yaml:"next_selector"`
	MaxPages     int      `yaml:"max_pages"`

	// links only
	URLs []string `yaml:"urls"`
}

// Accept reports whether a discovered URL passes the include/exclude filters.
func (d *Discovery) Accept(u string) bool {
	for _, r := range d.Exclude {
		if r.Re.MatchString(u) {
			return false
		}
	}
	if len(d.Include) == 0 {
		return true
	}
	for _, r := range d.Include {
		if r.Re.MatchString(u) {
			return true
		}
	}
	return false
}

// GUID modes (§7.1).
const (
	GUIDPermalink  = "permalink"
	GUIDStableHash = "stable-hash"
)

type Item struct {
	Canonical  []Source  `yaml:"canonical"`
	Title      []Source  `yaml:"title"`
	Summary    []Source  `yaml:"summary"`
	Author     []Source  `yaml:"author"`
	Image      []Source  `yaml:"image"`
	Categories []Source  `yaml:"categories"`
	Published  DateField `yaml:"published"`
	Updated    DateField `yaml:"updated"`
	Content    Content   `yaml:"content"`
	GUID       string    `yaml:"guid"`
	Enclosure  bool      `yaml:"enclosure"` // emit <enclosure> for the lead image (§6.9)
}

type DateField struct {
	Sources []Source `yaml:"sources"`
	Layouts []Layout `yaml:"layouts"`
}

type Content struct {
	Selector       Selector   `yaml:"selector"`
	Exclude        []Selector `yaml:"exclude"`
	KeepFirstImage *bool      `yaml:"keep_first_image"`
	KeepVideo      bool       `yaml:"keep_video"`
}

// Defaults applied to unset site fields.
var (
	DefaultInterval      = time.Hour
	DefaultRefreshWindow = 14 * 24 * time.Hour
	DefaultMaxItems      = 50
	DefaultTimezone      = "UTC"
	DefaultCanonical     = []string{"meta:og:url", "css:link[rel=canonical]@href", "request"}
)

func (s *Site) RespectRobots() bool { return s.Fetch.RespectRobots == nil || *s.Fetch.RespectRobots }
func (s *Site) KeepFirstImage() bool {
	return s.Item.Content.KeepFirstImage == nil || *s.Item.Content.KeepFirstImage
}
