package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the relay's YAML file. ${VAR} references are expanded from the
// environment before parsing, so secrets can stay out of the file.
type Config struct {
	Hibira        HibiraConfig `yaml:"hibira"`
	Listen        string       `yaml:"listen"`         // health, console and WebSub callbacks, ":8080"
	PublicURL     string       `yaml:"public_url"`     // base URL hubs can reach, enables WebSub
	StateFile     string       `yaml:"state_file"`     // sent item IDs
	FeedsFile     string       `yaml:"feeds_file"`     // feeds added in the web console
	AdminPassword string       `yaml:"admin_password"` // turns on the web console at /
	MCPToken      string       `yaml:"mcp_token"`      // turns on the MCP server at /mcp
	RSSHubURL     string       `yaml:"rsshub_url"`     // lets a feed URL be just an RSSHub route, "/github/release/a/b"
	Defaults      FeedDefaults `yaml:"defaults"`
	Feeds         []Feed       `yaml:"feeds"`
}

type HibiraConfig struct {
	URL            string `yaml:"url"` // https://ws.hibira.io
	AppID          string `yaml:"app_id"`
	Key            string `yaml:"key"`
	Secret         string `yaml:"secret"`
	PushInstanceID string `yaml:"push_instance_id"`
	PushSecretKey  string `yaml:"push_secret_key"`
}

type FeedDefaults struct {
	Interval  Duration `yaml:"interval"`
	Event     string   `yaml:"event"`
	MaxBytes  int      `yaml:"max_bytes"`
	MaxItems  int      `yaml:"max_items"`
	UserAgent string   `yaml:"user_agent"`
}

type Feed struct {
	Name     string            `yaml:"name" json:"name"`
	URL      string            `yaml:"url" json:"url"`
	Channels []string          `yaml:"channels" json:"channels,omitempty"`
	Channel  string            `yaml:"channel" json:"-"` // shorthand for one channel
	Event    string            `yaml:"event" json:"event,omitempty"`
	Interval Duration          `yaml:"interval" json:"interval,omitempty"`
	Backfill int               `yaml:"backfill" json:"backfill,omitempty"` // items to send on the very first fetch
	Headers  map[string]string `yaml:"headers" json:"headers,omitempty"`
	WebSub   *bool             `yaml:"websub" json:"websub,omitempty"` // default on when public_url is set
	Push     *PushTarget       `yaml:"push" json:"push,omitempty"`
	Inbox    *InboxTarget      `yaml:"inbox" json:"inbox,omitempty"`
	Disabled bool              `yaml:"disabled" json:"disabled,omitempty"`

	// Source is "file" for feeds from the config file (read-only in the
	// console) and "console" for feeds added in the console.
	Source string `yaml:"-" json:"-"`
	// FetchURL is URL with an RSSHub route resolved against rsshub_url.
	FetchURL string `yaml:"-" json:"-"`
}

type PushTarget struct {
	Interests []string `yaml:"interests" json:"interests,omitempty"`
	Users     []string `yaml:"users" json:"users,omitempty"`
	Title     string   `yaml:"title" json:"title,omitempty"` // default: the feed's title
}

type InboxTarget struct {
	Users    []string `yaml:"users" json:"users"`
	Category string   `yaml:"category" json:"category,omitempty"`
	Push     string   `yaml:"push" json:"push,omitempty"` // offline (default), always, never
}

// Duration reads "5m", "90s" or a number of seconds.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	return d.parse(s)
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var secs float64
		if err := json.Unmarshal(b, &secs); err != nil {
			return fmt.Errorf("invalid duration %s", b)
		}
		*d = Duration(time.Duration(secs * float64(time.Second)))
		return nil
	}
	return d.parse(s)
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// String is "5m", "1h30m" or "45s", without zero units.
func (d Duration) String() string {
	s := time.Duration(d).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func (d *Duration) parse(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		*d = 0
		return nil
	}
	if v, err := time.ParseDuration(s); err == nil {
		*d = Duration(v)
		return nil
	}
	var secs int
	if _, err := fmt.Sscanf(s, "%d", &secs); err == nil && fmt.Sprint(secs) == s {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}
	return fmt.Errorf("invalid duration %q", s)
}

var (
	channelRe = regexp.MustCompile(`^[A-Za-z0-9_\-=@,.;#]{1,164}$`)
	envRe     = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

const (
	defaultURL      = "https://ws.hibira.io"
	defaultInterval = 5 * time.Minute
	minInterval     = 30 * time.Second
	defaultMaxBytes = 3500
	minPasswordLen  = 12
	minTokenLen     = 24
)

// LoadConfig reads and checks the file at path.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(raw)
}

func ParseConfig(raw []byte) (*Config, error) {
	expanded := envRe.ReplaceAllStringFunc(string(raw), func(m string) string {
		return os.Getenv(envRe.FindStringSubmatch(m)[1])
	})
	var c Config
	if err := yaml.Unmarshal([]byte(expanded), &c); err != nil {
		return nil, err
	}
	c.applyEnv()
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

// applyEnv fills settings the file leaves empty from the environment.
func (c *Config) applyEnv() {
	set := func(dst *string, key string) {
		if *dst == "" {
			*dst = os.Getenv(key)
		}
	}
	set(&c.Hibira.URL, "HIBIRA_URL")
	set(&c.Hibira.AppID, "HIBIRA_APP_ID")
	set(&c.Hibira.Key, "HIBIRA_APP_KEY")
	set(&c.Hibira.Secret, "HIBIRA_APP_SECRET")
	set(&c.Hibira.PushInstanceID, "HIBIRA_PUSH_INSTANCE_ID")
	set(&c.Hibira.PushSecretKey, "HIBIRA_PUSH_SECRET_KEY")
	set(&c.PublicURL, "PUBLIC_URL")
	set(&c.StateFile, "STATE_FILE")
	set(&c.FeedsFile, "FEEDS_FILE")
	set(&c.Listen, "LISTEN")
	set(&c.AdminPassword, "ADMIN_PASSWORD")
	set(&c.MCPToken, "MCP_TOKEN")
	set(&c.RSSHubURL, "RSSHUB_URL")
}

func (c *Config) normalize() error {
	var errs []error
	if c.Hibira.URL == "" {
		c.Hibira.URL = defaultURL
	}
	if !strings.Contains(c.Hibira.URL, "://") {
		c.Hibira.URL = "https://" + c.Hibira.URL
	}
	c.Hibira.URL = strings.TrimRight(c.Hibira.URL, "/")
	if c.Hibira.AppID == "" || c.Hibira.Key == "" || c.Hibira.Secret == "" {
		errs = append(errs, errors.New("hibira app_id, key and secret are required (or HIBIRA_APP_ID, HIBIRA_APP_KEY, HIBIRA_APP_SECRET)"))
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.StateFile == "" {
		c.StateFile = "feed-relay-state.json"
	}
	if c.FeedsFile == "" {
		c.FeedsFile = filepath.Join(filepath.Dir(c.StateFile), "feeds.json")
	}
	if c.AdminPassword != "" && len(c.AdminPassword) < minPasswordLen {
		errs = append(errs, fmt.Errorf("admin_password must be at least %d characters", minPasswordLen))
	}
	if c.MCPToken != "" && len(c.MCPToken) < minTokenLen {
		errs = append(errs, fmt.Errorf("mcp_token must be at least %d characters (openssl rand -hex 24)", minTokenLen))
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.PublicURL != "" && !httpURL(c.PublicURL) {
		errs = append(errs, fmt.Errorf("public_url %q must be an http(s) URL", c.PublicURL))
	}
	c.RSSHubURL = strings.TrimRight(c.RSSHubURL, "/")
	if c.RSSHubURL != "" && !httpURL(c.RSSHubURL) {
		errs = append(errs, fmt.Errorf("rsshub_url %q must be an http(s) URL", c.RSSHubURL))
	}
	d := &c.Defaults
	if d.Interval == 0 {
		d.Interval = Duration(defaultInterval)
	}
	if d.Event == "" {
		d.Event = "new-item"
	}
	if d.MaxBytes <= 0 {
		d.MaxBytes = defaultMaxBytes
	}
	if d.MaxItems <= 0 {
		d.MaxItems = 20
	}
	if d.UserAgent == "" {
		d.UserAgent = "hibira-feed-relay/" + Version + " (+https://github.com/hibira-io/feed-relay)"
	}
	if len(c.Feeds) == 0 && c.AdminPassword == "" && c.MCPToken == "" {
		errs = append(errs, errors.New("add at least one feed under feeds:, or set admin_password (ADMIN_PASSWORD) or mcp_token (MCP_TOKEN) to add feeds at runtime"))
	}
	names := map[string]bool{}
	for i := range c.Feeds {
		f := &c.Feeds[i]
		f.Source = "file"
		if err := c.CheckFeed(f); err != nil {
			errs = append(errs, fmt.Errorf("feeds[%d]: %w", i, err))
		}
		if names[f.Name] {
			errs = append(errs, fmt.Errorf("feeds[%d]: name %q is used twice", i, f.Name))
		}
		names[f.Name] = true
	}
	return errors.Join(errs...)
}

// CheckFeed fills a feed's defaults and reports what is wrong with it.
func (c *Config) CheckFeed(f *Feed) error {
	var errs []error
	f.Name = strings.TrimSpace(f.Name)
	f.URL = strings.TrimSpace(f.URL)
	f.FetchURL = f.URL
	if strings.HasPrefix(f.URL, "/") {
		if c.RSSHubURL == "" {
			errs = append(errs, fmt.Errorf("url %q is an RSSHub route: set rsshub_url (RSSHUB_URL) or use a full URL", f.URL))
		}
		f.FetchURL = c.RSSHubURL + f.URL
	}
	if !strings.HasPrefix(f.URL, "/") && !httpURL(f.URL) {
		errs = append(errs, fmt.Errorf("url %q must be an http(s) URL or an RSSHub route starting with /", f.URL))
	}
	if f.Name == "" {
		f.Name = f.URL
	}
	if len(f.Name) > 200 {
		errs = append(errs, errors.New("name longer than 200 characters"))
	}
	if f.Channel != "" {
		f.Channels = append([]string{f.Channel}, f.Channels...)
		f.Channel = ""
	}
	f.Channels = compact(f.Channels)
	if len(f.Channels) > 100 {
		errs = append(errs, errors.New("at most 100 channels"))
	}
	for _, ch := range f.Channels {
		if !channelRe.MatchString(ch) {
			errs = append(errs, fmt.Errorf("channel %q has characters a channel name cannot use", ch))
		}
	}
	f.Event = strings.TrimSpace(f.Event)
	if f.Event == "" {
		f.Event = c.Defaults.Event
	}
	if len(f.Event) > 200 {
		errs = append(errs, errors.New("event name longer than 200 characters"))
	}
	if f.Interval == 0 {
		f.Interval = c.Defaults.Interval
	}
	if time.Duration(f.Interval) < minInterval {
		f.Interval = Duration(minInterval)
	}
	if f.Backfill < 0 {
		f.Backfill = 0
	}
	if f.Push != nil {
		f.Push.Interests, f.Push.Users = compact(f.Push.Interests), compact(f.Push.Users)
		if len(f.Push.Interests) == 0 && len(f.Push.Users) == 0 {
			errs = append(errs, errors.New("push needs interests or users"))
		}
		if c.Hibira.PushInstanceID == "" || c.Hibira.PushSecretKey == "" {
			errs = append(errs, errors.New("push needs hibira push_instance_id and push_secret_key (HIBIRA_PUSH_INSTANCE_ID, HIBIRA_PUSH_SECRET_KEY, from the app's Push tab)"))
		}
	}
	if f.Inbox != nil {
		f.Inbox.Users = compact(f.Inbox.Users)
		if len(f.Inbox.Users) == 0 || len(f.Inbox.Users) > 1000 {
			errs = append(errs, errors.New("inbox needs 1 to 1,000 users"))
		}
		switch f.Inbox.Push {
		case "", "offline", "always", "never":
		default:
			errs = append(errs, errors.New("inbox push must be offline, always or never"))
		}
	}
	if len(f.Channels) == 0 && f.Push == nil && f.Inbox == nil {
		errs = append(errs, errors.New("set channel(s), push or inbox, or the feed goes nowhere"))
	}
	return errors.Join(errs...)
}

// webSubOn reports whether the relay subscribes to the feed's hub.
func (c *Config) webSubOn(f *Feed) bool {
	if c.PublicURL == "" {
		return false
	}
	return f.WebSub == nil || *f.WebSub
}

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// compact trims entries and drops empty and repeated ones.
func compact(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
