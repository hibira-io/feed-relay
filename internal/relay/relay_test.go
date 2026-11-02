package relay

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSignMatchesPusherDocs(t *testing.T) {
	q := url.Values{}
	q.Set("auth_key", "278d425bdf160c739803")
	q.Set("auth_timestamp", "1353088179")
	q.Set("auth_version", "1.0")
	q.Set("body_md5", "ec365a775a4cd0599faeb73354201b6f")
	got := Sign("7ad3773142a6692b25b8", "POST", "/apps/3/events", q)
	if want := "da454824c97ba181a32ccc17a72625ba02771f50b50e1e7430e47a1f3f457e6c"; got != want {
		t.Fatalf("signature %s, want %s", got, want)
	}
}

// fakeHibira records what the relay sends and checks every signature.
type fakeHibira struct {
	t        *testing.T
	mu       sync.Mutex
	events   []map[string]any
	inbox    []map[string]any
	pushes   []map[string]any
	paths    []string
	failWith int // answer this status to the next request
	srv      *httptest.Server
}

func newFakeHibira(t *testing.T) *fakeHibira {
	h := &fakeHibira{t: t}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.paths = append(h.paths, r.URL.Path)
		if h.failWith != 0 {
			w.WriteHeader(h.failWith)
			h.failWith = 0
			return
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Errorf("body is not JSON: %s", body)
		}
		if strings.HasPrefix(r.URL.Path, "/publish_api/") {
			if r.Header.Get("Authorization") != "Bearer push-secret" {
				t.Errorf("push without the secret key")
			}
			m["_to"] = r.URL.Path
			h.pushes = append(h.pushes, m)
			w.Write([]byte(`{"publishId":"p1"}`))
			return
		}
		q := r.URL.Query()
		sum := md5.Sum(body)
		if q.Get("body_md5") != hex.EncodeToString(sum[:]) {
			t.Errorf("body_md5 mismatch")
		}
		if q.Get("auth_signature") != Sign("app-secret-0123456789", r.Method, r.URL.Path, q) {
			t.Errorf("bad signature on %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/apps/42/events":
			h.events = append(h.events, m)
			w.Write([]byte(`{}`))
		case "/apps/42/inbox/notifications":
			h.inbox = append(h.inbox, m)
			w.WriteHeader(201)
			w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHibira) count() (int, int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.events), len(h.inbox), len(h.pushes)
}

// fakeFeed serves an RSS document with the given items and an ETag.
type fakeFeed struct {
	mu      sync.Mutex
	items   []string // item titles, newest first
	extra   string   // extra XML in <channel>
	hits    int
	notMods int
	srv     *httptest.Server
}

func newFakeFeed(t *testing.T, titles ...string) *fakeFeed {
	f := &fakeFeed{items: titles}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.hits++
		etag := fmt.Sprintf(`"v%d"`, len(f.items))
		if r.Header.Get("If-None-Match") == etag {
			f.notMods++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, rss(f.extra, f.items))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFeed) add(title string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append([]string{title}, f.items...)
}

func rss(extra string, titles []string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom"><channel><title>Release notes</title><link>https://example.com/</link>` + extra)
	for i, title := range titles {
		n := len(titles) - i
		fmt.Fprintf(&b, `<item><title>%s</title><link>https://example.com/p/%d</link><guid>post-%d</guid><description>&lt;p&gt;Body of &lt;b&gt;%s&lt;/b&gt;&lt;/p&gt;</description><pubDate>%s</pubDate></item>`,
			title, n, n, title, time.Date(2026, 11, 1, 0, n, 0, 0, time.UTC).Format(time.RFC1123Z))
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

func testRelay(t *testing.T, h *fakeHibira, yamlFeeds string) *Relay {
	t.Helper()
	cfg, err := ParseConfig([]byte(fmt.Sprintf(`
hibira:
  url: %s
  app_id: "42"
  key: app-key
  secret: app-secret-0123456789
  push_instance_id: inst-1
  push_secret_key: push-secret
state_file: %s
feeds:
%s`, h.srv.URL, filepath.Join(t.TempDir(), "state.json"), yamlFeeds)))
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	r := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.sleep = func(ctx context.Context, d time.Duration) bool { return ctx.Err() == nil }
	return r
}

func TestFirstFetchPrimesThenSendsNewItems(t *testing.T) {
	h := newFakeHibira(t)
	feed := newFakeFeed(t, "Two", "One")
	r := testRelay(t, h, fmt.Sprintf("  - name: notes\n    url: %s\n    channel: releases\n    event: release\n", feed.srv.URL))
	ctx := context.Background()

	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := h.count(); n != 0 {
		t.Fatalf("first fetch sent %d events, want 0 (existing items are only remembered)", n)
	}
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if feed.notMods != 1 {
		t.Fatalf("second fetch did not use the ETag (304s: %d)", feed.notMods)
	}

	feed.add("Three & more")
	feed.add("Four")
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.events) != 2 {
		t.Fatalf("sent %d events, want 2", len(h.events))
	}
	first := h.events[0]
	if first["name"] != "release" || first["channels"].([]any)[0] != "releases" {
		t.Fatalf("event %v", first)
	}
	var ev Event
	if err := json.Unmarshal([]byte(first["data"].(string)), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Title != "Three & more" || ev.ID != "post-3" || ev.Link != "https://example.com/p/3" || ev.Feed.Name != "notes" || ev.Feed.Title != "Release notes" {
		t.Fatalf("oldest new item should go first, got %+v", ev)
	}
	if ev.Summary != "Body of Three & more" {
		t.Fatalf("summary should be plain text, got %q", ev.Summary)
	}
	if ev.Published != "2026-11-01T00:03:00Z" {
		t.Fatalf("published %q", ev.Published)
	}

	// Nothing new: nothing sent, even after a restart from the state file.
	store, err := OpenStore(r.cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	r2 := New(r.cfg, store, r.log)
	feed.mu.Lock()
	feed.items = append(feed.items, "") // change the ETag, same items
	feed.items = feed.items[:len(feed.items)-1]
	feed.mu.Unlock()
	if err := r2.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := h.count(); n != 2 {
		t.Fatalf("restart re-sent items: %d events", n)
	}
}

func TestBackfillAndMaxItems(t *testing.T) {
	h := newFakeHibira(t)
	feed := newFakeFeed(t, "C", "B", "A")
	r := testRelay(t, h, fmt.Sprintf("  - url: %s\n    channel: news\n    backfill: 2\n", feed.srv.URL))
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.events) != 2 {
		t.Fatalf("backfill sent %d, want 2", len(h.events))
	}
	if !strings.Contains(h.events[0]["data"].(string), `"title":"B"`) || h.events[0]["name"] != "new-item" {
		t.Fatalf("backfill should send the 2 newest, oldest first: %v", h.events[0])
	}

	r.cfg.Defaults.MaxItems = 2
	for _, s := range []string{"D", "E", "F", "G"} {
		feed.add(s)
	}
	if err := r.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.events) != 4 || !strings.Contains(h.events[2]["data"].(string), `"title":"F"`) {
		t.Fatalf("a burst should send only the newest max_items: %d events", len(h.events))
	}
}

func TestRetryAndRefusedItems(t *testing.T) {
	h := newFakeHibira(t)
	feed := newFakeFeed(t, "A")
	r := testRelay(t, h, fmt.Sprintf("  - url: %s\n    channel: news\n", feed.srv.URL))
	ctx := context.Background()
	_ = r.Once(ctx)

	// A 413 is final: the item is skipped, not retried forever.
	feed.add("Too big")
	h.failWith = 413
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	feed.add("Next")
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.events) != 1 || !strings.Contains(h.events[0]["data"].(string), `"title":"Next"`) {
		t.Fatalf("events %v", h.events)
	}

	// A 503 is retried within the same fetch.
	feed.add("Busy")
	h.failWith = 503
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.events) != 2 {
		t.Fatalf("503 should be retried: %d events", len(h.events))
	}
}

func TestPushAndInbox(t *testing.T) {
	h := newFakeHibira(t)
	feed := newFakeFeed(t, "A")
	r := testRelay(t, h, fmt.Sprintf(`  - name: blog
    url: %s
    push:
      interests: [blog]
      title: New post
    inbox:
      users: [ann, bob]
      category: blog
`, feed.srv.URL))
	ctx := context.Background()
	_ = r.Once(ctx)
	feed.add("Hello world")
	if err := r.Once(ctx); err != nil {
		t.Fatal(err)
	}
	ev, in, pu := h.count()
	if ev != 0 || in != 1 || pu != 1 {
		t.Fatalf("events %d inbox %d push %d", ev, in, pu)
	}
	n := h.inbox[0]
	if n["title"] != "Hello world" || n["url"] != "https://example.com/p/2" || n["category"] != "blog" || !strings.HasPrefix(n["key"].(string), "feed-relay:") {
		t.Fatalf("inbox %v", n)
	}
	p := h.pushes[0]
	if p["_to"] != "/publish_api/v1/instances/inst-1/publishes/interests" || p["interests"].([]any)[0] != "blog" {
		t.Fatalf("push %v", p)
	}
	web := p["web"].(map[string]any)["notification"].(map[string]any)
	if web["title"] != "New post" || web["body"] != "Hello world" || web["deep_link"] != "https://example.com/p/2" {
		t.Fatalf("web notification %v", web)
	}
}

func TestEventFitsMaxBytes(t *testing.T) {
	f := &Feed{Name: "x"}
	p := &Parsed{Title: "Feed"}
	it := Item{ID: "1", Title: strings.Repeat("標題", 400), Summary: strings.Repeat("內容\"<>", 2000), Link: "https://example.com/1"}
	out := EventJSON(f, p, it, 3500)
	if len(out) > 3500 {
		t.Fatalf("event is %d bytes", len(out))
	}
	// the summary goes first; a title that alone is too long is cut next
	it.Title = strings.Repeat("標題", 1000)
	if out := EventJSON(f, p, it, 3500); len(out) > 3500 || !strings.Contains(out, "…") {
		t.Fatalf("long title: %d bytes", len(out))
	}
	if len(out) > 3500 {
		t.Fatalf("event is %d bytes", len(out))
	}
	var ev Event
	if err := json.Unmarshal([]byte(out), &ev); err != nil {
		t.Fatalf("truncated event is not valid JSON: %v", err)
	}
	if ev.Link != it.Link || ev.ID != "1" || ev.Title != strings.Repeat("標題", 400) {
		t.Fatalf("unexpected %+v", ev)
	}
}

func TestParseAtomAndJSONFeed(t *testing.T) {
	atom := `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>Videos</title>
<link rel="hub" href="https://pubsubhubbub.appspot.com"/><link rel="self" href="https://www.youtube.com/feeds/videos.xml?channel_id=UC1"/>
<entry><id>yt:video:2</id><title>Second</title><link href="https://youtu.be/2"/><updated>2026-11-02T00:00:00Z</updated><author><name>Ann</name></author></entry>
<entry><id>yt:video:1</id><title>First</title><link href="https://youtu.be/1"/><updated>2026-11-01T00:00:00Z</updated></entry></feed>`
	p, err := Parse([]byte(atom))
	if err != nil {
		t.Fatal(err)
	}
	if p.Hub != "https://pubsubhubbub.appspot.com" || p.Self != "https://www.youtube.com/feeds/videos.xml?channel_id=UC1" {
		t.Fatalf("hub %q self %q", p.Hub, p.Self)
	}
	if len(p.Items) != 2 || p.Items[0].ID != "yt:video:1" || p.Items[1].Author != "Ann" {
		t.Fatalf("items %+v", p.Items)
	}
	jf := `{"version":"https://jsonfeed.org/version/1.1","title":"J","items":[{"id":"a","title":"A","url":"https://e.com/a","content_html":"<p>x</p>"}]}`
	p, err = Parse([]byte(jf))
	if err != nil || len(p.Items) != 1 || p.Items[0].Summary != "x" {
		t.Fatalf("json feed: %v %+v", err, p)
	}
	hub, self := linkHeader([]string{`<https://hub.example/>; rel="hub", <https://e.com/feed>; rel="self"`})
	if hub != "https://hub.example/" || self != "https://e.com/feed" {
		t.Fatalf("link header: %q %q", hub, self)
	}
}

func TestWebSub(t *testing.T) {
	h := newFakeHibira(t)
	var hubForm url.Values
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		hubForm = r.PostForm
		w.WriteHeader(http.StatusAccepted)
	}))
	defer hub.Close()
	feed := newFakeFeed(t, "A")
	feed.extra = `<atom:link rel="hub" href="` + hub.URL + `"/><atom:link rel="self" href="https://example.com/feed.xml"/>`
	r := testRelay(t, h, fmt.Sprintf("  - name: blog\n    url: %s\n    channel: news\n", feed.srv.URL))
	r.cfg.PublicURL = "https://relay.example.com"
	f := &r.cfg.Feeds[0]
	ctx := context.Background()

	if err := r.Poll(ctx, f); err != nil {
		t.Fatal(err)
	}
	r.renewWebSub(ctx, f)
	if hubForm.Get("hub.mode") != "subscribe" || hubForm.Get("hub.topic") != "https://example.com/feed.xml" ||
		hubForm.Get("hub.callback") != "https://relay.example.com/websub/"+hookID("blog") || hubForm.Get("hub.secret") == "" {
		t.Fatalf("subscribe form %v", hubForm)
	}
	hubForm = nil
	r.renewWebSub(ctx, f)
	if hubForm != nil {
		t.Fatal("subscribed again while the first request is pending")
	}

	handler := r.Handler()
	cb := "/websub/" + hookID("blog")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", cb+"?hub.mode=subscribe&hub.topic=https%3A%2F%2Fexample.com%2Ffeed.xml&hub.challenge=xyz&hub.lease_seconds=3600", nil))
	if rec.Code != 200 || rec.Body.String() != "xyz" {
		t.Fatalf("verify: %d %q", rec.Code, rec.Body.String())
	}
	if lease := time.Until(r.store.Get("blog").LeaseUntil); lease < 59*time.Minute {
		t.Fatalf("lease not stored: %v", lease)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", cb+"?hub.mode=subscribe&hub.topic=https%3A%2F%2Fevil.example%2F&hub.challenge=xyz", nil))
	if rec.Code != 404 {
		t.Fatalf("verify for another topic answered %d", rec.Code)
	}

	push := func(body, sig string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", cb, strings.NewReader(body))
		req.Header.Set("X-Hub-Signature", sig)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("content answered %d", rec.Code)
		}
	}
	body := rss("", []string{"Pushed", "A"})
	push(body, "sha256=00")
	time.Sleep(100 * time.Millisecond)
	if n, _, _ := h.count(); n != 0 {
		t.Fatal("content with a bad signature was sent")
	}
	mac := hmac.New(sha256.New, []byte(r.hubSecret(f)))
	mac.Write([]byte(body))
	push(body, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	deadline := time.Now().Add(2 * time.Second)
	for {
		if n, _, _ := h.count(); n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pushed item was not sent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.mu.Lock()
	data := h.events[0]["data"].(string)
	h.mu.Unlock()
	if !strings.Contains(data, `"title":"Pushed"`) {
		t.Fatalf("sent %s", data)
	}
}

func TestConfigErrors(t *testing.T) {
	t.Setenv("HIBIRA_APP_ID", "1")
	t.Setenv("HIBIRA_APP_KEY", "k")
	t.Setenv("HIBIRA_APP_SECRET", "s")
	t.Setenv("FEED_URL", "https://rsshub.example/github/release/a/b")
	c, err := ParseConfig([]byte("feeds:\n  - url: ${FEED_URL}\n    channel: releases\n    interval: 10s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Feeds[0].URL != "https://rsshub.example/github/release/a/b" || c.Hibira.URL != "https://ws.hibira.io" || time.Duration(c.Feeds[0].Interval) != 30*time.Second {
		t.Fatalf("config %+v", c)
	}
	_, err = ParseConfig([]byte("feeds:\n  - url: ftp://x\n    channel: bad channel\n  - url: https://a.example/\n    push: {interests: [x]}\n"))
	for _, want := range []string{"must be an http(s) URL", "cannot use", "push_instance_id"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v should mention %q", err, want)
		}
	}
}
