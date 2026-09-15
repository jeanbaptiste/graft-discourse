package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"time"
)

// Mapping is an explicit, admin-controlled link between a Discourse topic
// and a Graft note. It is the trusted alternative to title matching.
type Mapping struct {
	TopicID int64  `json:"topic_id"`
	NoteURI string `json:"note_uri"`
}

// Config is the bridge's on-disk configuration (JSON, dependency-free).
type Config struct {
	Listen        string   `json:"listen"`
	PublicBaseURL string   `json:"public_base_url"`
	ActorName     string   `json:"actor_name"`
	StateFile     string   `json:"state_file"`
	RepoURL       string   `json:"repo_url"`
	PollInterval  Duration `json:"poll_interval"`
	PassTimeout   Duration `json:"pass_timeout"`

	// Mappings are trusted topic<->issue links. Preferred over any
	// heuristic.
	Mappings []Mapping `json:"mappings"`
	// AllowTitleMatching enables the heuristic fallback (topic title equals
	// issue title). Off by default: it lets anyone who can create a topic
	// impersonate a repo issue.
	AllowTitleMatching bool `json:"allow_title_matching"`
	// AllowInsecureHTTP permits a plaintext discourse.base_url. Off by
	// default; only enable when Discourse is on a trusted internal
	// network. Deliberately scoped to Discourse alone: public_base_url
	// and graft.base_url must always be HTTPS — Graft's own SSRF guard
	// rejects a plaintext actor/inbox URL outright, and relaxing them
	// here would just produce a bridge Graft refuses to talk to, or an
	// actor endpoint an attacker could intercept in transit.
	AllowInsecureHTTP bool `json:"allow_insecure_http"`
	// AdminToken enables the /admin/mappings API when non-empty. Prefer the
	// GRAFT_BRIDGE_ADMIN_TOKEN environment variable over storing it here.
	AdminToken string `json:"admin_token"`
	// AllowedCategories, when non-empty, restricts forwarding to topics in
	// these Discourse category ids.
	AllowedCategories []int64 `json:"allowed_categories"`
	// MaxContentRunes caps the size of a single mirrored message.
	MaxContentRunes int `json:"max_content_runes"`
	// MaxDeliveriesPerPass bounds outbound comments per pass (rate limit).
	MaxDeliveriesPerPass int `json:"max_deliveries_per_pass"`

	Graft struct {
		BaseURL string   `json:"base_url"`
		Series  []string `json:"series"`
	} `json:"graft"`

	Discourse struct {
		BaseURL string `json:"base_url"`
		// PublicURL is the browser-facing Discourse URL used in trackback
		// links; defaults to BaseURL.
		PublicURL   string `json:"public_url"`
		APIKey      string `json:"api_key"`
		APIUsername string `json:"api_username"`
		MaxPosts    int    `json:"max_posts"`
	} `json:"discourse"`
}

// Duration is a time.Duration that unmarshals from a Go duration string.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// D returns the duration as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Load reads, validates and defaults a config file. A group- or
// world-readable file is refused because it contains the Discourse API key.
func Load(path string) (*Config, error) {
	if info, err := os.Stat(path); err == nil {
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			return nil, fmt.Errorf("config file %s is accessible to other users (mode %04o); run: chmod 600 %s", path, mode, path)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.ActorName == "" {
		c.ActorName = "discourse"
	}
	if c.StateFile == "" {
		c.StateFile = "./state.json"
	}
	if c.PollInterval == 0 {
		c.PollInterval = Duration(30 * time.Second)
	}
	if c.PassTimeout == 0 {
		c.PassTimeout = Duration(2 * time.Minute)
	}
	if c.Discourse.MaxPosts == 0 {
		c.Discourse.MaxPosts = 50
	}
	if c.MaxContentRunes == 0 {
		c.MaxContentRunes = 8000
	}
	if c.MaxDeliveriesPerPass == 0 {
		c.MaxDeliveriesPerPass = 20
	}

	if c.PublicBaseURL == "" {
		return nil, fmt.Errorf("public_base_url is required (Graft fetches our actor over HTTPS)")
	}
	if c.Graft.BaseURL == "" {
		return nil, fmt.Errorf("graft.base_url is required")
	}
	if len(c.Graft.Series) == 0 {
		return nil, fmt.Errorf("graft.series must list at least one series")
	}
	if c.Discourse.BaseURL == "" {
		return nil, fmt.Errorf("discourse.base_url is required")
	}

	// public_base_url and graft.base_url are never allowed to relax to
	// HTTP, regardless of allow_insecure_http — that flag exists only for
	// discourse.base_url. See the field's doc comment.
	for name, raw := range map[string]string{
		"public_base_url": c.PublicBaseURL,
		"graft.base_url":  c.Graft.BaseURL,
	} {
		if err := checkScheme(name, raw, false); err != nil {
			return nil, err
		}
	}
	if err := checkScheme("discourse.base_url", c.Discourse.BaseURL, c.AllowInsecureHTTP); err != nil {
		return nil, err
	}

	return &c, nil
}

func checkScheme(name, raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if allowHTTP {
			return nil
		}
		return fmt.Errorf("%s must use https (set allow_insecure_http only for a trusted internal network)", name)
	default:
		return fmt.Errorf("%s has unsupported scheme %q", name, u.Scheme)
	}
}
