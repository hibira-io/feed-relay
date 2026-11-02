package relay

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the relay's YAML file. ${VAR} references are expanded from the
// environment before parsing, so secrets can stay out of the file.
type Config struct {
	Hibira    HibiraConfig `yaml:"hibira"`
	Listen    string       `yaml:"listen"`     // health and WebSub callbacks, ":8080"
	PublicURL string       `yaml:"public_url"` // base URL hubs can reach, enables WebSub
	StateFile string       `yaml:"state_file"`
	Defaults  FeedDefaults `yaml:"defaults"`
	Feeds     []Feed       `yaml:"feeds"`
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
	Name     string            `yaml:"name"`
	URL      string            `yaml:"url"`
	Channels []string          `yaml:"channels"`
	Channel  string            `yaml:"channel"` // shorthand for one channel
	Event    string            `yaml:"event"`
	Interval Duration          `yaml:"interval"`
	Backfill int               `yaml:"backfill"` // items to send on the very first fetch
	Headers  map[string]string `yaml:"headers"`
	WebSub   *bool             `yaml:"websub"` // default on when public_url is set
	Push     *PushTarget       `yaml:"push"`
	Inbox    *InboxTarget      `yaml:"inbox"`
}

type PushTarget struct {
	Interests []string `yaml:"interests"`
	Users     []string `yaml:"users"`
	Title     string   `yaml:"title"` // default: the feed's title
}

type InboxTarget struct {
	Users    []string `yaml:"users"`
	Category string   `yaml:"category"`
	Push     string   `yaml:"push"` // offline (default), always, never
}

// Duration reads "5m", "90s" or a number of seconds.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	if v, err := time.ParseDuration(s); err == nil {
		*d = Duration(v)
		return nil
	}
	var secs int
	if err := n.Decode(&secs); err == nil {
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

// applyEnv fills credentials the file leaves empty from HIBIRA_* variables.
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
	set(&c.Listen, "LISTEN")
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
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.PublicURL != "" {
		if u, err := url.Parse(c.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("public_url %q must be an http(s) URL", c.PublicURL))
		}
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
	if len(c.Feeds) == 0 {
		errs = append(errs, errors.New("add at least one feed under feeds:"))
	}
	names := map[string]bool{}
	for i := range c.Feeds {
		f := &c.Feeds[i]
		where := fmt.Sprintf("feeds[%d]", i)
		if u, err := url.Parse(f.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s: url %q must be an http(s) URL", where, f.URL))
		}
		if f.Name == "" {
			f.Name = f.URL
		}
		if names[f.Name] {
			errs = append(errs, fmt.Errorf("%s: name %q is used twice", where, f.Name))
		}
		names[f.Name] = true
		if f.Channel != "" {
			f.Channels = append([]string{f.Channel}, f.Channels...)
		}
		if len(f.Channels) > 100 {
			errs = append(errs, fmt.Errorf("%s: at most 100 channels", where))
		}
		for _, ch := range f.Channels {
			if !channelRe.MatchString(ch) {
				errs = append(errs, fmt.Errorf("%s: channel %q has characters a channel name cannot use", where, ch))
			}
		}
		if f.Event == "" {
			f.Event = d.Event
		}
		if len(f.Event) > 200 {
			errs = append(errs, fmt.Errorf("%s: event name longer than 200 characters", where))
		}
		if f.Interval == 0 {
			f.Interval = d.Interval
		}
		if time.Duration(f.Interval) < minInterval {
			f.Interval = Duration(minInterval)
		}
		if f.Backfill < 0 {
			f.Backfill = 0
		}
		if f.Push != nil {
			if len(f.Push.Interests) == 0 && len(f.Push.Users) == 0 {
				errs = append(errs, fmt.Errorf("%s: push needs interests or users", where))
			}
			if c.Hibira.PushInstanceID == "" || c.Hibira.PushSecretKey == "" {
				errs = append(errs, fmt.Errorf("%s: push needs hibira push_instance_id and push_secret_key (from the app's Push tab)", where))
			}
		}
		if f.Inbox != nil {
			if len(f.Inbox.Users) == 0 || len(f.Inbox.Users) > 1000 {
				errs = append(errs, fmt.Errorf("%s: inbox needs 1 to 1,000 users", where))
			}
			switch f.Inbox.Push {
			case "", "offline", "always", "never":
			default:
				errs = append(errs, fmt.Errorf("%s: inbox push must be offline, always or never", where))
			}
		}
		if len(f.Channels) == 0 && f.Push == nil && f.Inbox == nil {
			errs = append(errs, fmt.Errorf("%s: set channel(s), push or inbox, or the feed goes nowhere", where))
		}
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
