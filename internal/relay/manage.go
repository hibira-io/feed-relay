package relay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Managing feeds at runtime, shared by the web console and the MCP server.
// Feeds added this way are saved in feeds_file; feeds from the config file
// can be read, checked and tested, but not changed.

var (
	ErrNotFound = errors.New("no feed with that name")
	ErrReadOnly = errors.New("this feed is set in the config file; change it there")
	ErrExists   = errors.New("a feed with that name already exists")
	ErrNoItems  = errors.New("the feed has no items")
)

// ProblemsError lists what is wrong with a feed.
type ProblemsError struct{ Problems []string }

func (e *ProblemsError) Error() string { return "fix these first: " + strings.Join(e.Problems, "; ") }

func problems(err error) error {
	if err == nil {
		return nil
	}
	return &ProblemsError{Problems: strings.Split(err.Error(), "\n")}
}

// Managed reports whether feeds can be changed at runtime.
func (r *Relay) Managed() bool { return r.console != nil }

// FeedInfo is a feed with what the relay knows about it.
type FeedInfo struct {
	ID     string     `json:"id"`
	Source string     `json:"source"`
	Feed   *Feed      `json:"feed"`
	Status feedStatus `json:"status"`
	Hub    string     `json:"hub,omitempty"`
}

func (r *Relay) info(f *Feed) FeedInfo {
	return FeedInfo{ID: hookID(f.Name), Source: f.Source, Feed: f, Status: r.statusOf(f.Name), Hub: r.store.Get(f.Name).Hub}
}

// FeedInfos lists every feed.
func (r *Relay) FeedInfos() []FeedInfo {
	out := []FeedInfo{}
	for _, f := range r.Feeds() {
		out = append(out, r.info(f))
	}
	return out
}

// FilterFeeds lists the feeds whose name, URL, channels or event contain
// every word of query, and whose status is status ("running", "paused",
// "failing" or empty for any). It returns limit of them from offset, and the
// number that matched.
func (r *Relay) FilterFeeds(query, status string, offset, limit int) ([]FeedInfo, int) {
	words := strings.Fields(strings.ToLower(query))
	var matched []FeedInfo
	for _, v := range r.FeedInfos() {
		f := v.Feed
		switch status {
		case "running":
			if f.Disabled {
				continue
			}
		case "paused":
			if !f.Disabled {
				continue
			}
		case "failing":
			if f.Disabled || v.Status.LastError == "" {
				continue
			}
		}
		text := strings.ToLower(strings.Join(append([]string{f.Name, f.URL, f.Event}, f.Channels...), " "))
		ok := true
		for _, w := range words {
			if !strings.Contains(text, w) {
				ok = false
				break
			}
		}
		if ok {
			matched = append(matched, v)
		}
	}
	offset = min(max(offset, 0), len(matched))
	end := len(matched)
	if limit > 0 {
		end = min(offset+limit, end)
	}
	return append([]FeedInfo{}, matched[offset:end]...), len(matched)
}

// FindFeed finds a feed by name, or by the id the console uses.
func (r *Relay) FindFeed(nameOrID string) (*Feed, error) {
	if rn := r.feed(nameOrID); rn != nil {
		return rn.feed, nil
	}
	if f := r.feedByHook(nameOrID); f != nil {
		return f, nil
	}
	return nil, ErrNotFound
}

func (r *Relay) editable(nameOrID string) (*Feed, error) {
	f, err := r.FindFeed(nameOrID)
	if err != nil {
		return nil, err
	}
	if f.Source != "console" {
		return nil, ErrReadOnly
	}
	return f, nil
}

// AddFeed checks and starts a new feed. Its current items are remembered,
// not sent, unless it asks for a backfill.
func (r *Relay) AddFeed(f Feed, by string) (FeedInfo, error) {
	f.Source = "console"
	if err := r.cfg.CheckFeed(&f); err != nil {
		return FeedInfo{}, problems(err)
	}
	if r.feed(f.Name) != nil {
		return FeedInfo{}, fmt.Errorf("%w: %q", ErrExists, f.Name)
	}
	r.store.Delete(f.Name)
	r.put(f.Name, &f)
	r.announce(by, "added", f.Name, "")
	return r.info(&f), r.saveConsoleFeeds()
}

// UpdateFeed replaces a console feed. A new URL starts over: its current
// items are remembered again instead of sent.
func (r *Relay) UpdateFeed(nameOrID string, f Feed, by string) (FeedInfo, error) {
	old, err := r.editable(nameOrID)
	if err != nil {
		return FeedInfo{}, err
	}
	f.Source = "console"
	if err := r.cfg.CheckFeed(&f); err != nil {
		return FeedInfo{}, problems(err)
	}
	if f.Name != old.Name && r.feed(f.Name) != nil {
		return FeedInfo{}, fmt.Errorf("%w: %q", ErrExists, f.Name)
	}
	r.put(old.Name, &f)
	if f.FetchURL != old.FetchURL {
		r.store.Delete(f.Name)
		r.statusMu.Lock()
		delete(r.status, f.Name)
		r.statusMu.Unlock()
	}
	r.announce(by, "updated", f.Name, old.Name)
	return r.info(&f), r.saveConsoleFeeds()
}

// DeleteFeed stops a console feed and forgets it.
func (r *Relay) DeleteFeed(nameOrID string, by string) error {
	f, err := r.editable(nameOrID)
	if err != nil {
		return err
	}
	r.remove(f.Name)
	r.announce(by, "deleted", f.Name, "")
	if err := r.saveConsoleFeeds(); err != nil {
		return err
	}
	return r.store.Save()
}

// SetPaused pauses or resumes a console feed.
func (r *Relay) SetPaused(nameOrID string, paused bool, by string) (FeedInfo, error) {
	old, err := r.editable(nameOrID)
	if err != nil {
		return FeedInfo{}, err
	}
	f := *old
	f.Disabled = paused
	if !paused {
		if err := r.cfg.CheckFeed(&f); err != nil {
			return FeedInfo{}, problems(err)
		}
	}
	r.put(old.Name, &f)
	action := "resumed"
	if paused {
		action = "paused"
	}
	r.announce(by, action, f.Name, "")
	return r.info(&f), r.saveConsoleFeeds()
}

// CheckNow fetches a feed now and sends what is new.
func (r *Relay) CheckNow(ctx context.Context, nameOrID string) (int, FeedInfo, error) {
	f, err := r.FindFeed(nameOrID)
	if err != nil {
		return 0, FeedInfo{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	before := r.statusOf(f.Name).Sent
	err = r.Poll(ctx, f)
	return r.statusOf(f.Name).Sent - before, r.info(f), err
}

// SendTest sends the feed's newest item now, even if it was sent before.
func (r *Relay) SendTest(ctx context.Context, nameOrID string) (Item, *Feed, error) {
	f, err := r.FindFeed(nameOrID)
	if err != nil {
		return Item{}, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	res, err := Fetch(ctx, r.client, f, r.cfg.Defaults.UserAgent, "", "")
	if err != nil {
		return Item{}, f, fmt.Errorf("could not read the feed: %w", err)
	}
	if len(res.Feed.Items) == 0 {
		return Item{}, f, ErrNoItems
	}
	it := res.Feed.Items[len(res.Feed.Items)-1]
	if err := r.deliver(ctx, f, res.Feed, it); err != nil {
		return it, f, fmt.Errorf("hibira did not take it: %w", err)
	}
	return it, f, nil
}

// Preview is a feed read without saving it.
type Preview struct {
	Title    string        `json:"title"`
	Link     string        `json:"link,omitempty"`
	Count    int           `json:"count"`
	Hub      string        `json:"hub,omitempty"`
	FetchURL string        `json:"fetch_url"`
	MaxBytes int           `json:"max_bytes"`
	Items    []PreviewItem `json:"items"`
}

type PreviewItem struct {
	Item  Item `json:"item"`
	Bytes int  `json:"bytes"`
}

// PreviewFeed reads a feed URL (or RSSHub route) and returns its newest
// items with the size of the event each would become.
func (r *Relay) PreviewFeed(ctx context.Context, url string, headers map[string]string, limit int) (Preview, error) {
	f := &Feed{URL: url, Headers: headers, Channels: []string{"preview"}}
	if err := r.cfg.CheckFeed(f); err != nil {
		return Preview{}, problems(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := Fetch(ctx, r.client, f, r.cfg.Defaults.UserAgent, "", "")
	if err != nil {
		return Preview{}, fmt.Errorf("could not read the feed: %w", err)
	}
	p := res.Feed
	if limit <= 0 {
		limit = 5
	}
	out := Preview{Title: p.Title, Link: p.Link, Count: len(p.Items), Hub: p.Hub, FetchURL: f.FetchURL, MaxBytes: r.cfg.Defaults.MaxBytes, Items: []PreviewItem{}}
	for i := len(p.Items) - 1; i >= 0 && len(out.Items) < limit; i-- {
		it := p.Items[i]
		out.Items = append(out.Items, PreviewItem{Item: it, Bytes: len(EventJSON(f, p, it, r.cfg.Defaults.MaxBytes))})
	}
	return out, nil
}
