package relay

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestMatchPath(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          map[string]string
	}{
		{"/:user/:repo/releases", "/vercel/next.js/releases", map[string]string{"user": "vercel", "repo": "next.js"}},
		{"/:user/:repo/releases", "/vercel/next.js", nil},
		{"/:user/:repo?", "/vercel", map[string]string{"user": "vercel"}},
		{"/:user", "/vercel/next.js", nil},
		{"/channel/:id/*", "/channel/UC123/videos", map[string]string{"id": "UC123"}},
		{"/search?q=:q", "/search", map[string]string{}},
	}
	for _, c := range cases {
		got, ok := matchPath(c.pattern, c.path)
		if (c.want == nil) != !ok {
			t.Errorf("%s vs %s: ok=%v", c.pattern, c.path, ok)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s vs %s: %s=%q", c.pattern, c.path, k, got[k])
			}
		}
	}
	if r, ok := fillRoute("/github/release/:user/:repo", map[string]string{"user": "a", "repo": "b"}); !ok || r != "/github/release/a/b" {
		t.Errorf("fill: %q %v", r, ok)
	}
	if r, ok := fillRoute("/github/repos/:user/:type?/:sort?", map[string]string{"user": "a"}); !ok || r != "/github/repos/a" {
		t.Errorf("fill optional: %q %v", r, ok)
	}
	if _, ok := fillRoute("/github/release/:user/:repo", map[string]string{"user": "a"}); ok {
		t.Error("a missing required param should not fit")
	}
}

const fakeNamespaces = `{
  "github": {
    "name": "GitHub", "url": "github.com", "zh": {"name": "GitHub 中文"},
    "routes": {
      "/release/:user/:repo": {"path": "/release/:user/:repo", "name": "Repo Releases", "example": "/github/release/DIYgod/RSSHub",
        "parameters": {"user": "GitHub username", "repo": {"description": "Repository name", "default": "RSSHub"}},
        "zh": {"name": "仓库 Releases"}, "categories": ["programming"]},
      "/notifications": {"path": "/notifications", "name": "Notifications", "features": {"requireConfig": [{"name": "GITHUB_ACCESS_TOKEN", "description": "x"}]}},
      "/repos/:user/:type?": {"path": ["/repos/:user/:type?"], "name": "User Repo",
        "parameters": {"type": {"description": "Type", "options": [{"value": "all", "label": "All"}, {"value": "owner", "label": "Owner"}]}}}
    }
  },
  "youtube": {"name": "YouTube", "url": "www.youtube.com", "routes": {"/channel/:id": {"path": "/channel/:id", "name": "Channel"}}},
  "naughty": {"name": "Nope", "routes": {"/x": {"path": "/x", "name": "X", "features": {"nsfw": true}}}}
}`

const fakeRadar = `{
  "github.com": {"_name": "GitHub", ".": [
    {"title": "Repo Releases", "docs": "https://docs.rsshub.app/routes/github", "source": ["/:user/:repo/releases", "/:user/:repo/releases/*", "/:user/:repo"], "target": "/github/release/:user/:repo"},
    {"title": "User Repo", "docs": "https://docs.rsshub.app/routes/github", "source": ["/:user"], "target": "/github/repos/:user"}
  ]}
}`

func TestDiscoverAndRoutes(t *testing.T) {
	var namespaceHits int
	rsshub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/namespace":
			namespaceHits++
			io.WriteString(w, fakeNamespaces)
		case "/api/radar/rules":
			io.WriteString(w, fakeRadar)
		default:
			http.NotFound(w, r)
		}
	}))
	defer rsshub.Close()
	feed := newFakeFeed(t, "A")
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><html><head><title>My blog</title>
<link rel="alternate" type="application/rss+xml" title="Posts" href="/feed.xml">
<link rel="alternate" type="application/atom+xml" href="%s">
<link rel="stylesheet" href="/x.css">
</head><body><link rel="alternate" type="application/rss+xml" href="/ignored.xml"></body></html>`, feed.srv.URL)
	}))
	defer site.Close()

	cfg := &Config{RSSHubURL: rsshub.URL, Defaults: FeedDefaults{UserAgent: "test"}}
	r := New(cfg, &Store{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	d, err := r.Discover(ctx, site.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Feeds) != 2 || d.Feeds[0].URL != site.URL+"/feed.xml" || d.Feeds[0].Title != "Posts" || d.Feeds[1].Title != "My blog" || d.Feeds[1].Kind != "link" {
		t.Fatalf("page feeds: %+v", d.Feeds)
	}
	d, err = r.Discover(ctx, feed.srv.URL)
	if err != nil || len(d.Feeds) != 1 || d.Feeds[0].Kind != "feed" || d.Feeds[0].Title != "Release notes" {
		t.Fatalf("a feed address: %v %+v", err, d.Feeds)
	}

	// radar: github.com is not reachable from the test, so only routes come back
	u, _ := url.Parse("https://github.com/vercel/next.js/releases")
	found, err := r.radarFor(ctx, u)
	if err != nil || len(found) != 1 || found[0].URL != "/github/release/vercel/next.js" || found[0].Kind != "rsshub" {
		t.Fatalf("radar: %v %+v", err, found)
	}
	u, _ = url.Parse("https://www.github.com/vercel")
	if found, _ = r.radarFor(ctx, u); len(found) != 1 || found[0].URL != "/github/repos/vercel" {
		t.Fatalf("radar www: %+v", found)
	}

	routes, err := r.SearchRoutes(ctx, "github", "en", 10)
	if err != nil || len(routes) != 3 {
		t.Fatalf("search: %v %+v", err, routes)
	}
	if routes[len(routes)-1].Path != "/github/notifications" || !routes[len(routes)-1].NeedsConfig {
		t.Fatalf("routes that need config come last: %+v", routes)
	}
	var rel, repos RouteInfo
	for _, rt := range routes {
		switch rt.Path {
		case "/github/release/:user/:repo":
			rel = rt
		case "/github/repos/:user/:type?":
			repos = rt
		}
	}
	if len(rel.Params) != 2 || rel.Params[1].Default != "RSSHub" || rel.Params[0].Description != "GitHub username" {
		t.Fatalf("params: %+v", rel.Params)
	}
	if len(repos.Params) != 2 || !repos.Params[1].Optional || len(repos.Params[1].Options) != 2 {
		t.Fatalf("optional params: %+v", repos.Params)
	}
	if routes, _ = r.SearchRoutes(ctx, "", "en", 10); len(routes) != 3 || routes[0].Namespace != "github" {
		t.Fatalf("popular: %+v", routes)
	}
	if routes, _ = r.SearchRoutes(ctx, "release", "zh-CN", 10); len(routes) != 1 || routes[0].Name != "仓库 Releases" || routes[0].Site != "GitHub 中文" {
		t.Fatalf("zh: %+v", routes)
	}
	if routes, _ = r.SearchRoutes(ctx, "nope", "en", 10); len(routes) != 0 {
		t.Fatalf("nsfw routes are left out: %+v", routes)
	}
	if namespaceHits != 1 {
		t.Fatalf("the catalog should be kept: %d fetches", namespaceHits)
	}

	r2 := New(&Config{Defaults: FeedDefaults{UserAgent: "test"}}, &Store{}, r.log)
	if _, err := r2.SearchRoutes(ctx, "x", "en", 10); err != ErrNoRSSHub {
		t.Fatalf("without rsshub_url: %v", err)
	}
}

func TestBlockPrivateURLs(t *testing.T) {
	feed := newFakeFeed(t, "A")
	hub := newFakeFeed(t, "B")
	cfg := &Config{BlockPrivateURLs: true, RSSHubURL: hub.srv.URL, Defaults: FeedDefaults{UserAgent: "test"}}
	r := New(cfg, &Store{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if _, err := r.Discover(ctx, feed.srv.URL); err == nil || !strings.Contains(err.Error(), "private network") {
		t.Fatalf("a loopback page should be refused: %v", err)
	}
	if _, err := r.PreviewFeed(ctx, hub.srv.URL, nil, 1); err != nil {
		t.Fatalf("the rsshub_url host stays allowed: %v", err)
	}
	for _, a := range []string{"10.0.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "::1", "fd00::1", "::ffff:127.0.0.1"} {
		if !privateAddr(netip.MustParseAddr(a)) {
			t.Errorf("%s should be private", a)
		}
	}
	if privateAddr(netip.MustParseAddr("140.82.112.3")) {
		t.Error("a public address was refused")
	}
}
