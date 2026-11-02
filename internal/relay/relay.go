package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Version is set at build time with -ldflags "-X ...relay.Version=v0.1.0".
var Version = "dev"

// Relay polls feeds and sends new items to Hibira. Feeds come from the
// config file and from the web console, and can change while it runs.
type Relay struct {
	cfg    *Config
	hibira *Hibira
	store  *Store
	client *http.Client
	log    *slog.Logger

	mu      sync.Mutex
	feeds   map[string]*running // by name
	runCtx  context.Context     // set by Run; nil in Once mode
	runWG   sync.WaitGroup
	console *consoleFeeds

	statusMu sync.Mutex
	status   map[string]*feedStatus

	liveOnce sync.Once
	liveQ    chan string

	sleep func(context.Context, time.Duration) bool
}

type running struct {
	feed   *Feed
	lock   sync.Mutex
	cancel context.CancelFunc
}

type feedStatus struct {
	Name      string    `json:"name"`
	LastFetch time.Time `json:"last_fetch,omitzero"`
	LastSent  time.Time `json:"last_sent,omitzero"`
	Sent      int       `json:"sent"`
	LastError string    `json:"last_error,omitempty"`
	WebSub    string    `json:"websub,omitempty"`
}

// New builds a relay with the config file's feeds and, when the console is
// on, the feeds saved in feeds_file.
func New(cfg *Config, store *Store, log *slog.Logger) *Relay {
	if log == nil {
		log = slog.Default()
	}
	r := &Relay{
		cfg:    cfg,
		hibira: NewHibira(cfg.Hibira, nil),
		store:  store,
		client: &http.Client{Timeout: 30 * time.Second},
		log:    log,
		feeds:  map[string]*running{},
		status: map[string]*feedStatus{},
		sleep:  sleepCtx,
	}
	for i := range cfg.Feeds {
		f := cfg.Feeds[i]
		r.feeds[f.Name] = &running{feed: &f}
	}
	return r
}

// Feeds returns every feed, file feeds first, then by name.
func (r *Relay) Feeds() []*Feed {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Feed, 0, len(r.feeds))
	for _, rn := range r.feeds {
		out = append(out, rn.feed)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source == "file"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func (r *Relay) feed(name string) *running {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.feeds[name]
}

func (r *Relay) feedByHook(id string) *Feed {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, rn := range r.feeds {
		if hookID(name) == id {
			return rn.feed
		}
	}
	return nil
}

// Once fetches every enabled feed one time, for cron jobs and tests.
func (r *Relay) Once(ctx context.Context) error {
	var errs []error
	for _, f := range r.Feeds() {
		if f.Disabled {
			continue
		}
		if err := r.Poll(ctx, f); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Run polls every feed on its interval until ctx ends, serving health
// checks, the console and WebSub callbacks on cfg.Listen.
func (r *Relay) Run(ctx context.Context) error {
	srv := &http.Server{Addr: r.cfg.Listen, Handler: r.Handler(), ReadHeaderTimeout: 10 * time.Second}
	srvErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()
	r.mu.Lock()
	r.runCtx = ctx
	for _, rn := range r.feeds {
		r.startLocked(rn)
	}
	n := len(r.feeds)
	r.mu.Unlock()
	r.log.Info("feed relay started", "version", Version, "feeds", n, "listen", r.cfg.Listen,
		"websub", r.cfg.PublicURL != "", "console", r.cfg.AdminPassword != "")

	select {
	case <-ctx.Done():
	case err := <-srvErr:
		return err
	}
	r.runWG.Wait()
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shut)
	return r.store.Save()
}

// startLocked starts the feed's polling loop when the relay is running and
// the feed is enabled. r.mu must be held.
func (r *Relay) startLocked(rn *running) {
	if r.runCtx == nil || rn.feed.Disabled || rn.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(r.runCtx)
	rn.cancel = cancel
	f := rn.feed
	r.runWG.Add(1)
	go func() {
		defer r.runWG.Done()
		r.loop(ctx, f)
	}()
}

// put adds or replaces a feed (under oldName when it is renamed) and
// restarts its loop.
func (r *Relay) put(oldName string, f *Feed) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.feeds[oldName]; old != nil {
		if old.cancel != nil {
			old.cancel()
		}
		delete(r.feeds, oldName)
		if oldName != f.Name {
			r.store.Rename(oldName, f.Name)
			r.statusMu.Lock()
			if st := r.status[oldName]; st != nil {
				st.Name = f.Name
				r.status[f.Name] = st
				delete(r.status, oldName)
			}
			r.statusMu.Unlock()
		}
	}
	rn := &running{feed: f}
	r.feeds[f.Name] = rn
	r.startLocked(rn)
}

// remove stops a feed and forgets its state.
func (r *Relay) remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rn := r.feeds[name]; rn != nil {
		if rn.cancel != nil {
			rn.cancel()
		}
		delete(r.feeds, name)
	}
	r.store.Delete(name)
	r.statusMu.Lock()
	delete(r.status, name)
	r.statusMu.Unlock()
}

func (r *Relay) loop(ctx context.Context, f *Feed) {
	// spread the first fetches so many feeds do not start at once
	if !r.sleep(ctx, time.Duration(rand.Int64N(int64(2*time.Second)))) {
		return
	}
	failures := 0
	for {
		wait := time.Duration(f.Interval)
		if err := r.Poll(ctx, f); err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			// back off on a failing feed, up to 6× the interval
			wait = min(wait*time.Duration(1<<min(failures, 3)), 6*time.Duration(f.Interval))
		} else {
			failures = 0
		}
		r.renewWebSub(ctx, f)
		if !r.sleep(ctx, wait) {
			return
		}
	}
}

// Poll fetches the feed and sends the items it has not sent before.
func (r *Relay) Poll(ctx context.Context, f *Feed) error {
	st := r.store.Get(f.Name)
	res, err := Fetch(ctx, r.client, f, r.cfg.Defaults.UserAgent, st.ETag, st.LastModified)
	r.setStatus(f.Name, func(s *feedStatus) {
		s.LastFetch = time.Now().UTC()
		s.LastError = ""
		if err != nil {
			s.LastError = err.Error()
		}
	})
	if err != nil {
		r.log.Warn("fetch failed", "feed", f.Name, "err", err)
		return err
	}
	r.store.Update(f.Name, func(s *FeedState) {
		s.ETag, s.LastModified = res.ETag, res.LastModified
		if res.Feed != nil && res.Feed.Hub != "" && (s.Hub != res.Feed.Hub || s.Topic != res.Feed.Self) {
			s.Hub, s.Topic = res.Feed.Hub, res.Feed.Self
			s.LeaseUntil, s.RetryAt = time.Time{}, time.Time{}
		}
	})
	if res.NotModified {
		return nil
	}
	err = r.process(ctx, f, res.Feed)
	if serr := r.store.Save(); serr != nil {
		r.log.Error("saving state failed", "file", r.cfg.StateFile, "err", serr)
	}
	return err
}

// process sends the new items of p, oldest first.
func (r *Relay) process(ctx context.Context, f *Feed, p *Parsed) error {
	rn := r.feed(f.Name)
	if rn == nil || rn.feed != f {
		return nil // removed or replaced while this fetch was running
	}
	rn.lock.Lock()
	defer rn.lock.Unlock()

	var fresh []Item
	for _, it := range p.Items {
		if !r.store.Seen(f.Name, it.ID) {
			fresh = append(fresh, it)
		}
	}
	st := r.store.Get(f.Name)
	if !st.Primed {
		// First fetch: remember what is already there, send only the backfill.
		keep := min(f.Backfill, len(fresh))
		r.store.MarkSeen(f.Name, ids(fresh[:len(fresh)-keep])...)
		fresh = fresh[len(fresh)-keep:]
		r.store.Update(f.Name, func(s *FeedState) { s.Primed = true })
		r.log.Info("feed primed", "feed", f.Name, "items", len(p.Items), "backfill", keep)
	}
	if max := r.cfg.Defaults.MaxItems; len(fresh) > max {
		r.log.Warn("too many new items, sending only the newest", "feed", f.Name, "new", len(fresh), "sent", max)
		r.store.MarkSeen(f.Name, ids(fresh[:len(fresh)-max])...)
		fresh = fresh[len(fresh)-max:]
	}
	for _, it := range fresh {
		if err := r.deliver(ctx, f, p, it); err != nil {
			var api *APIError
			if errors.As(err, &api) && !api.Retry() {
				// Hibira refused this item for good (too big, bad request):
				// skip it rather than retry it on every poll.
				r.log.Error("item refused, skipping", "feed", f.Name, "item", it.ID, "err", err)
				r.store.MarkSeen(f.Name, it.ID)
				r.setStatus(f.Name, func(s *feedStatus) { s.LastError = err.Error() })
				continue
			}
			r.log.Warn("send failed, will retry on the next fetch", "feed", f.Name, "item", it.ID, "err", err)
			r.setStatus(f.Name, func(s *feedStatus) { s.LastError = err.Error() })
			return err
		}
		r.store.MarkSeen(f.Name, it.ID)
		r.setStatus(f.Name, func(s *feedStatus) { s.Sent++; s.LastSent = time.Now().UTC() })
		r.log.Info("item sent", "feed", f.Name, "title", it.Title)
	}
	return nil
}

// Event is the JSON every channel receives for a new item.
type Event struct {
	Feed EventFeed `json:"feed"`
	Item
}

type EventFeed struct {
	Name  string `json:"name"`
	Title string `json:"title,omitempty"`
	Link  string `json:"link,omitempty"`
}

// EventJSON encodes the item, shortening summary and then title so the
// event stays within maxBytes (the Free plan allows 4 KB per event).
func EventJSON(f *Feed, p *Parsed, it Item, maxBytes int) string {
	ev := Event{Feed: EventFeed{Name: f.Name, Title: p.Title, Link: p.Link}, Item: it}
	enc := func() string { b, _ := json.Marshal(ev); return string(b) }
	out := enc()
	for _, field := range []*string{&ev.Summary, &ev.Title, &ev.Author, &ev.Image} {
		for len(out) > maxBytes && *field != "" {
			// escaped JSON can be longer than the text, so cut and measure again
			over := len(out) - maxBytes
			*field = truncate(*field, max(len(*field)-over-8, 0))
			out = enc()
		}
	}
	return out
}

func (r *Relay) deliver(ctx context.Context, f *Feed, p *Parsed, it Item) error {
	if len(f.Channels) > 0 {
		data := EventJSON(f, p, it, r.cfg.Defaults.MaxBytes)
		if err := r.retry(ctx, func() error { return r.hibira.Trigger(ctx, f.Channels, f.Event, data) }); err != nil {
			return err
		}
	}
	title := firstNonEmpty(it.Title, it.Summary, it.Link)
	if f.Inbox != nil {
		n := map[string]any{
			"users": f.Inbox.Users,
			"title": truncate(title, 200),
			"key":   "feed-relay:" + hookID(f.Name+"\x00"+it.ID),
			"data":  map[string]any{"feed": f.Name, "id": truncate(it.ID, 1000)},
		}
		if it.Summary != "" && it.Summary != title {
			n["body"] = truncate(it.Summary, 2000)
		}
		if it.Link != "" {
			n["url"] = it.Link
		}
		if strings.HasPrefix(it.Image, "https://") || strings.HasPrefix(it.Image, "http://") {
			n["icon"] = it.Image
		}
		if f.Inbox.Category != "" {
			n["category"] = f.Inbox.Category
		}
		if f.Inbox.Push != "" {
			n["push"] = f.Inbox.Push
		}
		if err := r.retry(ctx, func() error { return r.hibira.Inbox(ctx, n) }); err != nil {
			return err
		}
	}
	if f.Push != nil {
		heading := firstNonEmpty(f.Push.Title, p.Title, f.Name)
		body := truncate(title, 300)
		for _, t := range []struct {
			to   string
			list []string
		}{{"interests", f.Push.Interests}, {"users", f.Push.Users}} {
			if len(t.list) == 0 {
				continue
			}
			if err := r.retry(ctx, func() error {
				return r.hibira.Push(ctx, t.to, t.list, heading, body, it.Link)
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// retry tries fn up to 3 times on network errors, 429 and 5xx.
func (r *Relay) retry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := range 3 {
		if err = fn(); err == nil {
			return nil
		}
		var api *APIError
		if errors.As(err, &api) && !api.Retry() {
			return err
		}
		if attempt == 2 {
			break
		}
		if !r.sleep(ctx, time.Duration(attempt+1)*time.Second) {
			return ctx.Err()
		}
	}
	return err
}

// Handler serves /healthz, the WebSub callbacks and, with an admin
// password, the web console.
func (r *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		feeds := make([]feedStatus, 0)
		for _, f := range r.Feeds() {
			feeds = append(feeds, r.statusOf(f.Name))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"version": Version, "feeds": feeds})
	})
	mux.HandleFunc("/websub/{id}", r.webSubCallback)
	if r.cfg.AdminPassword != "" {
		r.mountConsole(mux)
	}
	if r.cfg.MCPToken != "" {
		mux.Handle("/mcp", r.mcpHandler())
	}
	return mux
}

func (r *Relay) setStatus(name string, fn func(*feedStatus)) {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	st := r.status[name]
	if st == nil {
		st = &feedStatus{Name: name}
		r.status[name] = st
	}
	fn(st)
}

func (r *Relay) statusOf(name string) feedStatus {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	if st := r.status[name]; st != nil {
		return *st
	}
	return feedStatus{Name: name}
}

func ids(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

// hookID is a short stable id for a name.
func hookID(name string) string {
	h := sha256.Sum256([]byte(name))
	return hex.EncodeToString(h[:12])
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
