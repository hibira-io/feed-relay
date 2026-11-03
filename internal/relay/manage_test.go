package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	testPassword = "correct-horse-battery"
	testToken    = "mcp-token-0123456789abcdef0123"
)

// managedRelay runs a relay with the console and MCP on, and one feed from
// the config file.
func managedRelay(t *testing.T, h *fakeHibira, fileFeedURL string) (*Relay, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := ParseConfig([]byte(fmt.Sprintf(`
hibira: {url: %s, app_id: "42", key: app-key, secret: app-secret-0123456789}
state_file: %s
admin_password: %s
mcp_token: %s
feeds:
  - name: from file
    url: %s
    channel: file
`, h.srv.URL, filepath.Join(dir, "state.json"), testPassword, testToken, fileFeedURL)))
	if err != nil {
		t.Fatal(err)
	}
	store, _ := OpenStore(cfg.StateFile)
	r := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.sleep = func(ctx context.Context, d time.Duration) bool { return ctx.Err() == nil }
	if err := r.LoadConsoleFeeds(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	return r, srv
}

type consoleClient struct {
	t   *testing.T
	c   *http.Client
	url string
}

func (c *consoleClient) do(method, path string, body any, csrf bool) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, c.url+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set("X-Feed-Relay", "1")
	}
	res, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestConsoleAPI(t *testing.T) {
	h := newFakeHibira(t)
	fileFeed := newFakeFeed(t, "F1")
	feed := newFakeFeed(t, "B", "A")
	r, srv := managedRelay(t, h, fileFeed.srv.URL)
	jar, _ := cookiejar.New(nil)
	c := &consoleClient{t: t, c: &http.Client{Jar: jar}, url: srv.URL}

	res, err := http.Get(srv.URL + "/")
	if err != nil || res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("console page: %v %v", err, res)
	}
	if code, _ := c.do("GET", "/api/state", nil, false); code != 401 {
		t.Fatalf("state without a session: %d", code)
	}
	if code, _ := c.do("POST", "/api/login", map[string]string{"password": "nope-nope-nope"}, false); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	if code, _ := c.do("POST", "/api/login", map[string]string{"password": testPassword}, false); code != 204 {
		t.Fatalf("login: %d", code)
	}
	code, st := c.do("GET", "/api/state", nil, false)
	if code != 200 || len(st["feeds"].([]any)) != 1 {
		t.Fatalf("state: %d %v", code, st)
	}

	newFeed := map[string]any{"name": "blog", "url": feed.srv.URL, "channels": []string{"news"}, "interval": "1m"}
	if code, _ := c.do("POST", "/api/feeds", newFeed, false); code != 403 {
		t.Fatalf("a change without X-Feed-Relay should be refused: %d", code)
	}
	code, out := c.do("POST", "/api/feeds", newFeed, true)
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["id"].(string)
	if code, _ := c.do("POST", "/api/feeds", newFeed, true); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	if code, out := c.do("POST", "/api/feeds", map[string]any{"url": "ftp://x"}, true); code != 422 || len(out["problems"].([]any)) != 2 {
		t.Fatalf("invalid feed: %d %v", code, out)
	}
	raw, _ := os.ReadFile(r.cfg.FeedsFile)
	if !strings.Contains(string(raw), `"interval": "1m"`) || strings.Contains(string(raw), "from file") {
		t.Fatalf("feeds.json: %s", raw)
	}

	// check now primes, then a new item is sent
	if code, out := c.do("POST", "/api/feeds/"+id+"/poll", nil, true); code != 200 || out["sent"].(float64) != 0 {
		t.Fatalf("first check: %d %v", code, out)
	}
	feed.add("C")
	if code, out := c.do("POST", "/api/feeds/"+id+"/poll", nil, true); code != 200 || out["sent"].(float64) != 1 {
		t.Fatalf("second check: %d %v", code, out)
	}
	if code, out := c.do("POST", "/api/feeds/"+id+"/test", nil, true); code != 200 || out["item"].(map[string]any)["title"] != "C" {
		t.Fatalf("test: %d %v", code, out)
	}
	if n, _, _ := h.count(); n != 2 {
		t.Fatalf("events: %d", n)
	}

	// rename keeps what was sent
	renamed := map[string]any{"name": "blog 2", "url": feed.srv.URL, "channels": []string{"news"}}
	code, out = c.do("POST", "/api/feeds/"+id, renamed, true)
	if code != 200 {
		t.Fatalf("rename: %d %v", code, out)
	}
	id2 := out["id"].(string)
	if !r.store.Seen("blog 2", "post-3") {
		t.Fatal("rename lost the sent items")
	}
	if code, out := c.do("POST", "/api/feeds/"+id2+"/pause", map[string]bool{"paused": true}, true); code != 200 || out["feed"].(map[string]any)["disabled"] != true {
		t.Fatalf("pause: %d %v", code, out)
	}

	fileID := hookID("from file")
	if code, _ := c.do("POST", "/api/feeds/"+fileID, renamed, true); code != 409 {
		t.Fatalf("editing a config file feed: %d", code)
	}
	if code, _ := c.do("DELETE", "/api/feeds/"+fileID, nil, true); code != 409 {
		t.Fatalf("deleting a config file feed: %d", code)
	}
	if code, _ := c.do("DELETE", "/api/feeds/"+id2, nil, true); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if len(r.Feeds()) != 1 {
		t.Fatalf("feeds after delete: %d", len(r.Feeds()))
	}

	// a restart loads the console feeds again
	c.do("POST", "/api/feeds", newFeed, true)
	store, _ := OpenStore(r.cfg.StateFile)
	r2 := New(r.cfg, store, r.log)
	if err := r2.LoadConsoleFeeds(); err != nil || len(r2.Feeds()) != 2 {
		t.Fatalf("reload: %v %d", err, len(r2.Feeds()))
	}
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(req)
}

func TestMCP(t *testing.T) {
	h := newFakeHibira(t)
	fileFeed := newFakeFeed(t, "F1")
	feed := newFakeFeed(t, "B", "A")
	r, srv := managedRelay(t, h, fileFeed.srv.URL)
	ctx := context.Background()

	res, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil || res.StatusCode != 401 {
		t.Fatalf("no token: %v %v", err, res.StatusCode)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{testToken, http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if len(names) != 13 {
		t.Fatalf("tools: %v", names)
	}

	call := func(name string, args map[string]any) (map[string]any, bool) {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var out map[string]any
		if res.StructuredContent != nil {
			b, _ := json.Marshal(res.StructuredContent)
			_ = json.Unmarshal(b, &out)
		}
		if res.IsError {
			text := ""
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					text += tc.Text
				}
			}
			return map[string]any{"error": text}, true
		}
		return out, false
	}

	out, isErr := call("preview_feed", map[string]any{"url": feed.srv.URL, "limit": 1})
	if isErr || out["title"] != "Release notes" || len(out["items"].([]any)) != 1 {
		t.Fatalf("preview: %v", out)
	}
	out, isErr = call("add_feed", map[string]any{"name": "notes", "url": feed.srv.URL, "channels": []string{"news"}, "interval": "2m"})
	if isErr || out["feed"].(map[string]any)["interval"] != "2m" {
		t.Fatalf("add: %v", out)
	}
	if out, isErr = call("add_feed", map[string]any{"url": feed.srv.URL}); !isErr || !strings.Contains(out["error"].(string), "goes nowhere") {
		t.Fatalf("add without a target: %v", out)
	}
	out, _ = call("list_feeds", nil)
	if len(out["feeds"].([]any)) != 2 || out["total"].(float64) != 2 {
		t.Fatalf("list: %v", out)
	}
	if out, _ = call("list_feeds", map[string]any{"query": "NEWS"}); out["total"].(float64) != 1 || out["feeds"].([]any)[0].(map[string]any)["feed"].(map[string]any)["name"] != "notes" {
		t.Fatalf("list by channel: %v", out)
	}
	if out, _ = call("list_feeds", map[string]any{"offset": 1, "limit": 1}); len(out["feeds"].([]any)) != 1 || out["total"].(float64) != 2 {
		t.Fatalf("list page 2: %v", out)
	}
	if out, _ = call("list_feeds", map[string]any{"status": "paused"}); out["total"].(float64) != 0 {
		t.Fatalf("list paused: %v", out)
	}
	call("check_feed", map[string]any{"name": "notes"})
	feed.add("C")
	if out, _ = call("check_feed", map[string]any{"name": "notes"}); out["sent"].(float64) != 1 {
		t.Fatalf("check: %v", out)
	}
	if out, _ = call("send_test_item", map[string]any{"name": "notes"}); out["item"].(map[string]any)["title"] != "C" {
		t.Fatalf("test: %v", out)
	}
	if out, isErr = call("update_feed", map[string]any{"name": "from file", "feed": map[string]any{"url": feed.srv.URL, "channels": []string{"x"}}}); !isErr || !strings.Contains(out["error"].(string), "config file") {
		t.Fatalf("update a file feed: %v", out)
	}
	if out, isErr = call("pause_feed", map[string]any{"name": "notes", "paused": true}); isErr || out["feed"].(map[string]any)["disabled"] != true {
		t.Fatalf("pause: %v", out)
	}
	if _, isErr = call("delete_feed", map[string]any{"name": "notes"}); isErr || len(r.Feeds()) != 1 {
		t.Fatal("delete")
	}
	if n, _, _ := h.count(); n != 2 {
		t.Fatalf("events: %d", n)
	}
	live := h.liveEvents(t, 3)
	for _, ev := range live {
		if ev.By != "mcp" {
			t.Fatalf("live update not marked as from mcp: %+v", ev)
		}
	}
	if live[0].Action != "added" || live[0].Feed != "notes" || live[1].Action != "paused" || live[2].Action != "deleted" {
		t.Fatalf("live updates: %+v", live)
	}
}
