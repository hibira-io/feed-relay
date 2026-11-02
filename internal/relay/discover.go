package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"
	"golang.org/x/net/html"
	"golang.org/x/net/publicsuffix"
)

// Finding feeds, so people pick one instead of typing a URL:
//   - Discover reads a web page and lists the feeds it links to, plus the
//     RSSHub routes that match the page (RSSHub "radar" rules).
//   - SearchRoutes searches the RSSHub instance's route catalog.
// Both RSSHub parts need rsshub_url; they read /api/radar/rules and
// /api/namespace from that instance and keep them for a few hours.

// Found is a feed someone can pick.
type Found struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Kind  string `json:"kind"` // "feed": the address is a feed; "link": the page links to it; "rsshub": an RSSHub route for the page
	Docs  string `json:"docs,omitempty"`
}

// Discovery is what Discover found for an address.
type Discovery struct {
	Page  string  `json:"page"`
	Feeds []Found `json:"feeds"`
	Note  string  `json:"note,omitempty"`
	// PageError and RSSHubError say what could not be read, for the console
	// to put in its own words.
	PageError   string `json:"page_error,omitempty"`
	RSSHubError string `json:"rsshub_error,omitempty"`
}

// RouteInfo is one route in the RSSHub catalog.
type RouteInfo struct {
	Path        string       `json:"path"`
	Name        string       `json:"name"`
	Namespace   string       `json:"namespace"`
	Site        string       `json:"site"`
	SiteURL     string       `json:"site_url,omitempty"`
	Example     string       `json:"example,omitempty"`
	Description string       `json:"description,omitempty"`
	Params      []RouteParam `json:"params,omitempty"`
	Docs        string       `json:"docs"`
	NeedsConfig bool         `json:"needs_config,omitempty"`
	Categories  []string     `json:"categories,omitempty"`
}

type RouteParam struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Default     string        `json:"default,omitempty"`
	Optional    bool          `json:"optional,omitempty"`
	Options     []RouteOption `json:"options,omitempty"`
}

type RouteOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

var ErrNoRSSHub = errors.New("set rsshub_url to browse RSSHub routes")

const catalogTTL = 6 * time.Hour

type catalog struct {
	mu      sync.Mutex
	at      time.Time
	routes  map[string][]RouteInfo // by language: "en", "ja", "zh", "zh-TW"
	radarAt time.Time
	radar   map[string]radarDomain
}

type radarRule struct {
	Title  string   `json:"title"`
	Docs   string   `json:"docs"`
	Source []string `json:"source"`
	Target string   `json:"target"`
}

type radarDomain struct {
	Name  string
	Rules map[string][]radarRule // by subdomain, "." for none
}

// Discover lists the feeds an address leads to.
func (r *Relay) Discover(ctx context.Context, raw string) (Discovery, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Discovery{}, &ProblemsError{Problems: []string{"enter a web address"}}
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Discovery{}, &ProblemsError{Problems: []string{fmt.Sprintf("%q is not a web address", raw)}}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out := Discovery{Page: u.String(), Feeds: []Found{}}
	var radarErr error
	if r.cfg.RSSHubURL != "" {
		found, err := r.radarFor(ctx, u)
		radarErr = err
		out.Feeds = append(out.Feeds, found...)
	}
	page, err := r.feedsOnPage(ctx, u)
	if err != nil {
		if len(out.Feeds) == 0 {
			return out, fmt.Errorf("could not read %s: %w", u.Host, err)
		}
		out.PageError = err.Error()
		out.Note = fmt.Sprintf("Could not read the page itself (%v), so only RSSHub routes are listed.", err)
	}
	seen := map[string]bool{}
	for _, f := range out.Feeds {
		seen[f.URL] = true
	}
	for _, f := range page {
		if !seen[f.URL] {
			seen[f.URL] = true
			out.Feeds = append(out.Feeds, f)
		}
	}
	if radarErr != nil {
		out.RSSHubError = radarErr.Error()
	}
	if radarErr != nil && out.Note == "" {
		out.Note = fmt.Sprintf("RSSHub routes are not listed: %v.", radarErr)
	}
	return out, nil
}

// feedsOnPage reads the page: a feed itself, or the feeds its <link
// rel="alternate"> tags point to.
func (r *Relay) feedsOnPage(ctx context.Context, u *url.URL) ([]Found, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", r.cfg.Defaults.UserAgent)
	req.Header.Set("Accept", "text/html, application/rss+xml, application/atom+xml, application/feed+json, */*;q=0.5")
	res, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	base := res.Request.URL
	if feed, err := gofeed.NewParser().Parse(bytes.NewReader(body)); err == nil {
		title := strings.TrimSpace(feed.Title)
		if title == "" {
			title = base.Host
		}
		return []Found{{URL: base.String(), Title: title, Kind: "feed"}}, nil
	}
	return feedLinks(body, base), nil
}

var feedTypes = map[string]bool{
	"application/rss+xml":   true,
	"application/atom+xml":  true,
	"application/feed+json": true,
	"application/json":      true,
	"application/rdf+xml":   true,
	"text/xml":              true,
	"application/xml":       true,
}

func feedLinks(body []byte, base *url.URL) []Found {
	var out []Found
	z := html.NewTokenizer(bytes.NewReader(body))
	pageTitle := ""
	inTitle := false
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt == html.TextToken && inTitle && pageTitle == "" {
			pageTitle = strings.TrimSpace(string(z.Text()))
			continue
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken && tt != html.EndTagToken {
			continue
		}
		name, hasAttr := z.TagName()
		tag := string(name)
		if tag == "title" {
			inTitle = tt == html.StartTagToken
			continue
		}
		if tag == "body" {
			break
		}
		if tag != "link" || !hasAttr || tt == html.EndTagToken {
			continue
		}
		attrs := map[string]string{}
		for {
			k, v, more := z.TagAttr()
			attrs[strings.ToLower(string(k))] = string(v)
			if !more {
				break
			}
		}
		rels := strings.Fields(strings.ToLower(attrs["rel"]))
		typ := strings.ToLower(strings.TrimSpace(strings.Split(attrs["type"], ";")[0]))
		if !slices.Contains(rels, "alternate") || !feedTypes[typ] || attrs["href"] == "" {
			continue
		}
		ref, err := base.Parse(strings.TrimSpace(attrs["href"]))
		if err != nil || (ref.Scheme != "http" && ref.Scheme != "https") {
			continue
		}
		title := strings.TrimSpace(attrs["title"])
		out = append(out, Found{URL: ref.String(), Title: title, Kind: "link"})
	}
	for i := range out {
		if out[i].Title == "" {
			out[i].Title = pageTitle
		}
		if out[i].Title == "" {
			out[i].Title = base.Host
		}
	}
	return out
}

// radarFor lists the RSSHub routes whose radar rules match the address.
func (r *Relay) radarFor(ctx context.Context, u *url.URL) ([]Found, error) {
	rules, err := r.radarRules(ctx)
	if err != nil {
		return nil, err
	}
	host := strings.ToLower(u.Hostname())
	domain, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return nil, nil
	}
	d, ok := rules[domain]
	if !ok {
		return nil, nil
	}
	sub := strings.TrimSuffix(strings.TrimSuffix(host, domain), ".")
	if sub == "" {
		sub = "."
	}
	subs := []string{sub}
	if sub == "www" {
		subs = append(subs, ".")
	} else if sub == "." {
		subs = append(subs, "www")
	}
	var out []Found
	seen := map[string]bool{}
	for _, s := range subs {
		for _, rule := range d.Rules[s] {
			for _, src := range rule.Source {
				params, ok := matchPath(src, u.Path)
				if !ok {
					continue
				}
				target, ok := fillRoute(rule.Target, params)
				if !ok || seen[target] {
					continue
				}
				seen[target] = true
				title := rule.Title
				if d.Name != "" && !strings.Contains(strings.ToLower(title), strings.ToLower(d.Name)) {
					title = d.Name + " " + title
				}
				out = append(out, Found{URL: target, Title: title, Kind: "rsshub", Docs: rule.Docs})
				break
			}
		}
	}
	return out, nil
}

// matchPath matches an RSSHub radar source ("/:user/:repo/releases") against
// a URL path and returns the named segments.
func matchPath(pattern, path string) (map[string]string, bool) {
	pattern = stripQuery(pattern)
	ps := splitPath(pattern)
	xs := splitPath(path)
	params := map[string]string{}
	for i, p := range ps {
		if p == "*" {
			return params, true
		}
		if strings.HasPrefix(p, ":") {
			name, optional := paramName(p)
			if i >= len(xs) {
				if optional {
					continue
				}
				return nil, false
			}
			v, err := url.PathUnescape(xs[i])
			if err != nil {
				v = xs[i]
			}
			params[name] = v
			continue
		}
		if i >= len(xs) || !strings.EqualFold(p, xs[i]) {
			return nil, false
		}
	}
	return params, len(xs) <= len(ps)
}

// stripQuery drops a query or fragment from a pattern, but keeps the "?" that
// marks an optional parameter (":name?").
func stripQuery(p string) string {
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '#':
			return p[:i]
		case '?':
			seg := p[strings.LastIndex(p[:i], "/")+1 : i]
			if !strings.HasPrefix(seg, ":") {
				return p[:i]
			}
		}
	}
	return p
}

func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// paramName reads ":name", ":name?" and ":name{regex}?".
func paramName(seg string) (string, bool) {
	s := strings.TrimPrefix(seg, ":")
	optional := strings.HasSuffix(s, "?")
	s = strings.TrimSuffix(s, "?")
	if i := strings.Index(s, "{"); i >= 0 {
		s = s[:i]
	}
	return s, optional
}

// fillRoute puts params into an RSSHub route; optional ones without a value
// are dropped, and a missing required one means the route does not fit.
func fillRoute(route string, params map[string]string) (string, bool) {
	var out []string
	for _, seg := range splitPath(route) {
		if !strings.HasPrefix(seg, ":") {
			out = append(out, seg)
			continue
		}
		name, optional := paramName(seg)
		v, ok := params[name]
		if !ok || v == "" {
			if optional {
				continue
			}
			return "", false
		}
		out = append(out, url.PathEscape(v))
	}
	return "/" + strings.Join(out, "/"), true
}

func (r *Relay) rsshubGet(ctx context.Context, path string, v any) error {
	if r.cfg.RSSHubURL == "" {
		return ErrNoRSSHub
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.cfg.RSSHubURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", r.cfg.Defaults.UserAgent)
	req.Header.Set("Accept", "application/json")
	res, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach RSSHub: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("RSSHub answered HTTP %d for %s", res.StatusCode, path)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<20)).Decode(v); err != nil {
		return fmt.Errorf("RSSHub sent something unexpected for %s: %w", path, err)
	}
	return nil
}

func (r *Relay) radarRules(ctx context.Context) (map[string]radarDomain, error) {
	c := &r.catalog
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.radar != nil && time.Since(c.radarAt) < catalogTTL {
		return c.radar, nil
	}
	var raw map[string]map[string]json.RawMessage
	if err := r.rsshubGet(ctx, "/api/radar/rules", &raw); err != nil {
		if c.radar != nil {
			return c.radar, nil
		}
		return nil, err
	}
	out := make(map[string]radarDomain, len(raw))
	for domain, subs := range raw {
		d := radarDomain{Rules: map[string][]radarRule{}}
		for sub, v := range subs {
			if sub == "_name" {
				_ = json.Unmarshal(v, &d.Name)
				continue
			}
			var rules []radarRule
			if json.Unmarshal(v, &rules) == nil {
				d.Rules[sub] = rules
			}
		}
		out[domain] = d
	}
	c.radar, c.radarAt = out, time.Now()
	return out, nil
}

type nsItem struct {
	Name        string   `json:"name"`
	URL         string   `json:"url"`
	Description string   `json:"description"`
	Categories  []string `json:"categories"`
}

type nsRoute struct {
	Path        json.RawMessage            `json:"path"`
	Name        string                     `json:"name"`
	Example     string                     `json:"example"`
	Description string                     `json:"description"`
	Parameters  map[string]json.RawMessage `json:"parameters"`
	Categories  []string                   `json:"categories"`
	Features    struct {
		RequireConfig json.RawMessage `json:"requireConfig"`
		NSFW          bool            `json:"nsfw"`
	} `json:"features"`
	Ja   *nsRouteText `json:"ja"`
	Zh   *nsRouteText `json:"zh"`
	ZhTW *nsRouteText `json:"zh-TW"`
}

type nsRouteText struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	Parameters  map[string]json.RawMessage `json:"parameters"`
}

type nsEntry struct {
	nsItem
	Ja     *nsItem            `json:"ja"`
	Zh     *nsItem            `json:"zh"`
	ZhTW   *nsItem            `json:"zh-TW"`
	Routes map[string]nsRoute `json:"routes"`
}

var catalogLangs = []string{"en", "ja", "zh", "zh-TW"}

// catalogLang maps a console language to the closest RSSHub one.
func catalogLang(lang string) string {
	switch {
	case lang == "ja":
		return "ja"
	case lang == "zh-TW" || lang == "zh-HK" || lang == "zh-Hant":
		return "zh-TW"
	case strings.HasPrefix(lang, "zh"):
		return "zh"
	}
	return "en"
}

func (r *Relay) routeCatalog(ctx context.Context, lang string) ([]RouteInfo, error) {
	c := &r.catalog
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.routes != nil && time.Since(c.at) < catalogTTL {
		return c.routes[lang], nil
	}
	var raw map[string]nsEntry
	if err := r.rsshubGet(ctx, "/api/namespace", &raw); err != nil {
		if c.routes != nil {
			return c.routes[lang], nil
		}
		return nil, err
	}
	all := map[string][]RouteInfo{}
	for _, l := range catalogLangs {
		all[l] = buildCatalog(raw, l)
	}
	c.routes, c.at = all, time.Now()
	return all[lang], nil
}

func buildCatalog(raw map[string]nsEntry, lang string) []RouteInfo {
	var out []RouteInfo
	for ns, e := range raw {
		site := e.nsItem
		if alt := pickNS(e, lang); alt != nil && alt.Name != "" {
			site.Name = alt.Name
		}
		for key, rt := range e.Routes {
			if rt.Features.NSFW {
				continue
			}
			path := key
			var paths []string
			if json.Unmarshal(rt.Path, &paths) == nil && len(paths) > 0 {
				path = paths[0]
			} else {
				var p string
				if json.Unmarshal(rt.Path, &p) == nil && p != "" {
					path = p
				}
			}
			name, desc, params := rt.Name, rt.Description, rt.Parameters
			if t := pickRoute(rt, lang); t != nil {
				if t.Name != "" {
					name = t.Name
				}
				if t.Description != "" {
					desc = t.Description
				}
				if len(t.Parameters) > 0 {
					params = t.Parameters
				}
			}
			info := RouteInfo{
				Path:        "/" + ns + path,
				Name:        name,
				Namespace:   ns,
				Site:        site.Name,
				SiteURL:     site.URL,
				Example:     rt.Example,
				Description: truncate(clean(desc), 600),
				Docs:        "https://docs.rsshub.app/routes/" + ns,
				NeedsConfig: needsConfig(rt.Features.RequireConfig),
				Categories:  rt.Categories,
			}
			for _, seg := range splitPath(path) {
				if !strings.HasPrefix(seg, ":") {
					continue
				}
				pn, optional := paramName(seg)
				info.Params = append(info.Params, routeParam(pn, optional, params[pn]))
			}
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func pickNS(e nsEntry, lang string) *nsItem {
	switch lang {
	case "ja":
		return e.Ja
	case "zh":
		return e.Zh
	case "zh-TW":
		if e.ZhTW != nil {
			return e.ZhTW
		}
		return e.Zh
	}
	return nil
}

func pickRoute(rt nsRoute, lang string) *nsRouteText {
	switch lang {
	case "ja":
		return rt.Ja
	case "zh":
		return rt.Zh
	case "zh-TW":
		if rt.ZhTW != nil {
			return rt.ZhTW
		}
		return rt.Zh
	}
	return nil
}

func needsConfig(raw json.RawMessage) bool {
	var list []struct {
		Optional bool `json:"optional"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return false
	}
	for _, c := range list {
		if !c.Optional {
			return true
		}
	}
	return false
}

func routeParam(name string, optional bool, raw json.RawMessage) RouteParam {
	p := RouteParam{Name: name, Optional: optional}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		p.Description = truncate(clean(s), 300)
		return p
	}
	var obj struct {
		Description string        `json:"description"`
		Default     any           `json:"default"`
		Options     []RouteOption `json:"options"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		p.Description = truncate(clean(obj.Description), 300)
		if obj.Default != nil {
			p.Default = fmt.Sprint(obj.Default)
		}
		p.Options = obj.Options
	}
	return p
}

// popular namespaces come first when the search is empty.
var popular = []string{"github", "youtube", "telegram", "reddit", "bilibili", "twitter", "instagram", "spotify", "steam", "producthunt", "hackernews", "v2ex", "zhihu", "weibo", "pixiv", "nhk", "bbc"}

// RoutePage is one page of SearchRoutes results.
type RoutePage struct {
	Routes []RouteInfo `json:"routes"`
	Total  int         `json:"total"`
	Offset int         `json:"offset"`
}

// SearchRoutes finds RSSHub routes by site, name or path, best matches first,
// and returns limit of them starting at offset.
func (r *Relay) SearchRoutes(ctx context.Context, query, lang string, offset, limit int) (RoutePage, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	all, err := r.routeCatalog(ctx, catalogLang(lang))
	if err != nil {
		return RoutePage{}, err
	}
	offset = max(offset, 0)
	if limit <= 0 || limit > 100 {
		limit = 40
	}
	words := strings.Fields(strings.ToLower(query))
	type hit struct {
		score int
		i     int
	}
	var hits []hit
	for i, rt := range all {
		if rt.NeedsConfig && len(words) == 0 {
			continue
		}
		score := 0
		if len(words) == 0 {
			if k := slices.Index(popular, rt.Namespace); k >= 0 {
				score = 100 - k
			}
		}
		ok := true
		for _, w := range words {
			s := 0
			switch {
			case rt.Namespace == w || strings.EqualFold(rt.Site, w):
				s = 50
			case strings.Contains(strings.ToLower(rt.Site), w) || strings.Contains(rt.Namespace, w):
				s = 30
			case strings.Contains(strings.ToLower(rt.Name), w):
				s = 20
			case strings.Contains(strings.ToLower(rt.SiteURL), w) || strings.Contains(strings.ToLower(rt.Path), w):
				s = 10
			case slices.ContainsFunc(rt.Categories, func(c string) bool { return strings.Contains(c, w) }):
				s = 5
			}
			if s == 0 {
				ok = false
				break
			}
			score += s
		}
		if !ok || (len(words) == 0 && score == 0) {
			continue
		}
		if rt.NeedsConfig {
			score -= 3
		}
		hits = append(hits, hit{score, i})
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	out := RoutePage{Routes: []RouteInfo{}, Total: len(hits), Offset: offset}
	for _, h := range hits[min(offset, len(hits)):] {
		if len(out.Routes) == limit {
			break
		}
		out.Routes = append(out.Routes, all[h.i])
	}
	return out, nil
}
