package config

import "gopkg.in/yaml.v3"

// Global is feed-me.yaml (§4.1). Relative paths resolve against the file's directory.
type Global struct {
	SitesDir      string `yaml:"sites_dir"`
	StorePath     string `yaml:"store_path"`
	OutDir        string `yaml:"out_dir"`
	Listen        string `yaml:"listen"`
	PublicBaseURL string `yaml:"public_base_url"` // overridden by FEED_ME_PUBLIC_BASE_URL
	BasePath      string `yaml:"base_path"`
	ContactURL    string `yaml:"contact_url"` // goes in the User-Agent
	UserAgent     string `yaml:"user_agent"`  // replaces the default User-Agent entirely
	Log           Log    `yaml:"log"`

	// Fetch holds defaults for every site's fetch block.
	Fetch GlobalFetch `yaml:"fetch"`

	File string `yaml:"-"`
	root *yaml.Node
}

type Log struct {
	Format string `yaml:"format"` // json | text
	Level  string `yaml:"level"`  // debug | info | warn | error
}

type GlobalFetch struct {
	Rate         Rate     `yaml:"rate"`
	Timeout      Duration `yaml:"timeout"`
	MaxBodyBytes int64    `yaml:"max_body_bytes"`

	// AllowPrivateNetworks lets fetches reach loopback, private and other
	// non-public addresses, for sites on a LAN or a local test server. Off by
	// default: discovery follows URLs that remote content chooses.
	AllowPrivateNetworks bool `yaml:"allow_private_networks"`
}

const (
	DefaultSitesDir     = "sites"
	DefaultStorePath    = "feed-me.db"
	DefaultOutDir       = "out"
	DefaultListen       = ":8080"
	DefaultBasePath     = "/"
	DefaultRate         = "1/s"
	DefaultTimeout      = "20s"
	DefaultMaxBodyBytes = 10 << 20
	EnvPublicBaseURL    = "FEED_ME_PUBLIC_BASE_URL"
)
