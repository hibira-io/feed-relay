package relay

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

func TestExportImport(t *testing.T) {
	h := newFakeHibira(t)
	fileFeed := newFakeFeed(t, "F1")
	blog := newFakeFeed(t, "B", "A")
	notes := newFakeFeed(t, "N")
	_, srv := managedRelay(t, h, fileFeed.srv.URL)
	login := func(url string) *consoleClient {
		jar, _ := cookiejar.New(nil)
		c := &consoleClient{t: t, c: &http.Client{Jar: jar}, url: url}
		if code, _ := c.do("POST", "/api/login", map[string]string{"password": testPassword}, false); code != 204 {
			t.Fatalf("login: %d", code)
		}
		return c
	}
	c := login(srv.URL)
	for _, f := range []map[string]any{
		{"name": "blog", "url": blog.srv.URL, "channels": []string{"news"}, "interval": "2m", "headers": map[string]string{"X-Key": "k1"}},
		{"name": "notes", "url": notes.srv.URL, "channels": []string{"notes"}, "backfill": 3},
	} {
		if code, out := c.do("POST", "/api/feeds", f, true); code != 201 {
			t.Fatalf("create: %d %v", code, out)
		}
	}

	get := func(path string) (*http.Response, string) {
		res, err := c.c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}
	res, backup := get("/api/export")
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Disposition"), ".json") ||
		!strings.Contains(backup, `"X-Key": "k1"`) || !strings.Contains(backup, `"from file"`) || !strings.Contains(backup, `"interval": "2m"`) {
		t.Fatalf("json export: %d %s", res.StatusCode, backup)
	}
	res, opml := get("/api/export?format=opml")
	if !strings.Contains(res.Header.Get("Content-Type"), "opml") || !strings.Contains(opml, `xmlUrl="`+blog.srv.URL+`"`) || !strings.Contains(opml, `text="notes"`) {
		t.Fatalf("opml export: %s", opml)
	}
	if res, err := http.Get(srv.URL + "/api/export"); err != nil || res.StatusCode != 401 {
		t.Fatalf("export without a session: %v %v", err, res.StatusCode)
	}

	// a second relay, with the same config file feed
	r2, srv2 := managedRelay(t, h, fileFeed.srv.URL)
	c2 := login(srv2.URL)
	code, out := c2.do("POST", "/api/import", map[string]any{"data": backup, "dry_run": true}, true)
	if code != 200 || out["format"] != "json" || len(out["added"].([]any)) != 2 || len(out["skipped"].([]any)) != 1 ||
		out["skipped"].([]any)[0].(map[string]any)["reason"] != "exists" || len(r2.Feeds()) != 1 {
		t.Fatalf("dry run: %d %v", code, out)
	}
	if code, out = c2.do("POST", "/api/import", map[string]any{"data": backup}, true); code != 200 || len(out["added"].([]any)) != 2 || len(r2.Feeds()) != 3 {
		t.Fatalf("import: %d %v", code, out)
	}
	f, _ := r2.FindFeed("blog")
	if f.Headers["X-Key"] != "k1" || f.Interval.String() != "2m" || f.Source != "console" {
		t.Fatalf("imported feed: %+v", f)
	}
	if f, _ = r2.FindFeed("notes"); f.Backfill != 0 {
		t.Fatalf("an import must not backfill: %+v", f)
	}
	if code, out = c2.do("POST", "/api/import", map[string]any{"data": backup}, true); len(out["added"].([]any)) != 0 || len(out["skipped"].([]any)) != 3 {
		t.Fatalf("import again: %d %v", code, out)
	}
	if live := h.liveEvents(t, 3); live[2].Action != "imported" || live[2].Feed != "2" {
		t.Fatalf("live: %+v", live)
	}

	reader := `<?xml version="1.0"?><opml version="1.0"><head><title>Reader</title></head><body>
<outline text="Tech"><outline type="rss" text="Blog again" xmlUrl="` + blog.srv.URL + `"/>
<outline type="rss" title="Go" text="go" xmlUrl="https://go.dev/blog/feed.atom"/></outline>
<outline type="rss" text="Broken" xmlUrl="ftp://nope"/></body></opml>`
	code, out = c2.do("POST", "/api/import", map[string]any{"data": reader, "channels": []string{"reader"}}, true)
	if code != 200 || out["format"] != "opml" || len(out["added"].([]any)) != 1 || len(out["skipped"].([]any)) != 2 {
		t.Fatalf("opml import: %d %v", code, out)
	}
	sk := out["skipped"].([]any)
	if sk[0].(map[string]any)["reason"] != "same_url" || sk[0].(map[string]any)["other"] != "blog" || sk[1].(map[string]any)["reason"] != "invalid" {
		t.Fatalf("opml skipped: %v", sk)
	}
	if f, err := r2.FindFeed("Go"); err != nil || f.Channels[0] != "reader" {
		t.Fatalf("opml feed: %v %+v", err, f)
	}
	if code, out = c2.do("POST", "/api/import", map[string]any{"data": "hello"}, true); code != 422 || out["code"] != "format" {
		t.Fatalf("not a backup: %d %v", code, out)
	}
	if code, _ = c2.do("POST", "/api/import", map[string]any{"data": backup}, false); code != 403 {
		t.Fatalf("import without X-Feed-Relay: %d", code)
	}
}

func TestParseImportRoutes(t *testing.T) {
	r := New(&Config{RSSHubURL: "https://rsshub.example.com"}, &Store{}, nil)
	feeds, format, err := r.ParseImport([]byte("\xef\xbb\xbf  <opml><body><outline text=\"r\" xmlUrl=\"https://rsshub.example.com/github/release/a/b\"/></body></opml>"))
	if err != nil || format != "opml" || len(feeds) != 1 || feeds[0].URL != "/github/release/a/b" || feeds[0].Name != "r" {
		t.Fatalf("%v %s %+v", err, format, feeds)
	}
	if feeds, _, err = r.ParseImport([]byte(`[{"name":"x","url":"https://a.example/feed"}]`)); err != nil || len(feeds) != 1 {
		t.Fatalf("bare list: %v %+v", err, feeds)
	}
	if _, _, err = r.ParseImport([]byte(`{"hello":1}`)); err != ErrImportFormat {
		t.Fatalf("an object without feeds: %v", err)
	}
}
