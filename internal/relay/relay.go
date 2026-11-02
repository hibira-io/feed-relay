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
	"strings"
	"sync"
	"time"
)

// Version is set at build time with -ldflags "-X ...relay.Version=v0.1.0".
var Version = "dev"

// Relay polls feeds and sends new items to Hibira.
type Relay struct {
	cfg    *Config
	hibira *Hibira
	store  *Store
	client *http.Client
	log    *slog.Logger

	locks  map[string]*sync.Mutex
	byHook map[string]*Feed // WebSub callback id → feed

	statusMu sync.Mutex
	status   map[string]*feedStatus

	sleep func(context.Context, time.Duration) bool
}

type feedStatus struct {
	Name      string    `json:"name"`
	LastFetch time.Time `json:"last_fetch,omitzero"`
	LastSent  time.Time `json:"last_sent,omitzero"`
	Sent      int       `json:"sent"`
	LastError string    `json:"last_error,omitempty"`
	WebSub    string    `json:"websub,omitempty"`
}

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
		locks:  map[string]*sync.Mutex{},
		byHook: map[string]*Feed{},
		status: map[string]*feedStatus{},
		sleep:  sleepCtx,
	}
	for i := range cfg.Feeds {
		f := &cfg.Feeds[i]
		r.locks[f.Name] = &sync.Mutex{}
		r.byHook[hookID(f.Name)] = f
		r.status[f.Name] = &feedStatus{Name: f.Name}
	}
	return r
}

// Once fetches every feed one time, for cron jobs and tests.
func (r *Relay) Once(ctx context.Context) error {
	var errs []error
	for i := range r.cfg.Feeds {
		if err := r.Poll(ctx, &r.cfg.Feeds[i]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Run polls every feed on its interval until ctx ends, serving health
// checks and WebSub callbacks on cfg.Listen.
func (r *Relay) Run(ctx context.Context) error {
	srv := &http.Server{Addr: r.cfg.Listen, Handler: r.Handler(), ReadHeaderTimeout: 10 * time.Second}
	srvErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()
	r.log.Info("feed relay started", "version", Version, "feeds", len(r.cfg.Feeds), "listen", r.cfg.Listen, "websub", r.cfg.PublicURL != "")

	var wg sync.WaitGroup
	for i := range r.cfg.Feeds {
		f := &r.cfg.Feeds[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.loop(ctx, f)
		}()
	}
	select {
	case <-ctx.Done():
	case err := <-srvErr:
		return err
	}
	wg.Wait()
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shut)
	return r.store.Save()
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
	lock := r.locks[f.Name]
	lock.Lock()
	defer lock.Unlock()

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

// Handler serves /healthz and the WebSub callbacks.
func (r *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		r.statusMu.Lock()
		feeds := make([]feedStatus, 0, len(r.cfg.Feeds))
		for _, f := range r.cfg.Feeds {
			feeds = append(feeds, *r.status[f.Name])
		}
		r.statusMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"version": Version, "feeds": feeds})
	})
	mux.HandleFunc("/websub/{id}", r.webSubCallback)
	return mux
}

func (r *Relay) setStatus(name string, fn func(*feedStatus)) {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	fn(r.status[name])
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
