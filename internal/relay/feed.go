package relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mmcdole/gofeed"
)

// Item is one feed entry, normalized across RSS, Atom and JSON Feed.
type Item struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	Link      string `json:"link,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Author    string `json:"author,omitempty"`
	Image     string `json:"image,omitempty"`
	Published string `json:"published,omitempty"`
	published time.Time
}

// Parsed is a fetched feed.
type Parsed struct {
	Title string
	Link  string
	Items []Item // oldest first
	Hub   string // WebSub hub, if the feed advertises one
	Self  string // the topic URL to subscribe to
}

// FetchResult is what a conditional GET returned.
type FetchResult struct {
	NotModified  bool
	ETag         string
	LastModified string
	Feed         *Parsed
}

const maxFeedBytes = 10 << 20

var errTooBig = errors.New("feed is larger than 10 MB")

// Fetch GETs the feed, sending the validators from the last fetch.
func Fetch(ctx context.Context, client *http.Client, f *Feed, userAgent, etag, lastModified string) (*FetchResult, error) {
	target := f.FetchURL
	if target == "" {
		target = f.URL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/feed+json, application/json;q=0.9, application/xml;q=0.8, */*;q=0.5")
	for k, v := range f.Headers {
		req.Header.Set(k, v)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotModified {
		return &FetchResult{NotModified: true, ETag: etag, LastModified: lastModified}, nil
	}
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("feed answered %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxFeedBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxFeedBytes {
		return nil, errTooBig
	}
	p, err := Parse(body)
	if err != nil {
		return nil, err
	}
	if hub, self := linkHeader(res.Header.Values("Link")); hub != "" {
		p.Hub = hub
		if self != "" {
			p.Self = self
		}
	}
	if p.Self == "" {
		p.Self = target
	}
	return &FetchResult{ETag: res.Header.Get("ETag"), LastModified: res.Header.Get("Last-Modified"), Feed: p}, nil
}

// Parse reads an RSS, Atom or JSON feed.
func Parse(body []byte) (*Parsed, error) {
	feed, err := gofeed.NewParser().Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("not a feed: %w", err)
	}
	p := &Parsed{Title: clean(feed.Title), Link: feed.Link}
	p.Hub, p.Self = bodyLinks(body)
	for i, it := range feed.Items {
		item := Item{
			Title:   clean(it.Title),
			Link:    it.Link,
			Summary: clean(firstNonEmpty(it.Description, it.Content)),
		}
		if it.Author != nil {
			item.Author = clean(it.Author.Name)
		} else if len(it.Authors) > 0 && it.Authors[0] != nil {
			item.Author = clean(it.Authors[0].Name)
		}
		if it.Image != nil {
			item.Image = it.Image.URL
		} else {
			for _, e := range it.Enclosures {
				if e != nil && strings.HasPrefix(e.Type, "image/") {
					item.Image = e.URL
					break
				}
			}
		}
		switch {
		case it.PublishedParsed != nil:
			item.published = *it.PublishedParsed
		case it.UpdatedParsed != nil:
			item.published = *it.UpdatedParsed
		}
		if !item.published.IsZero() {
			item.Published = item.published.UTC().Format(time.RFC3339)
		}
		item.ID = itemID(it, i)
		p.Items = append(p.Items, item)
	}
	// Feeds list newest first; send oldest first. Keep document order for
	// items without dates.
	reverse(p.Items)
	sortStableByDate(p.Items)
	return p, nil
}

// itemID is the GUID, else the link, else a hash of title and date.
func itemID(it *gofeed.Item, i int) string {
	if s := strings.TrimSpace(it.GUID); s != "" {
		return s
	}
	if s := strings.TrimSpace(it.Link); s != "" {
		return s
	}
	h := sha256.Sum256([]byte(it.Title + "\x00" + it.Published + "\x00" + it.Description))
	return "sha256:" + hex.EncodeToString(h[:12])
}

func sortStableByDate(items []Item) {
	// insertion sort: feeds are small and this keeps undated items in place
	for i := 1; i < len(items); i++ {
		for j := i; j > 0; j-- {
			a, b := items[j-1].published, items[j].published
			if a.IsZero() || b.IsZero() || !a.After(b) {
				break
			}
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
}

func reverse(items []Item) {
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
}

var (
	linkTagRe = regexp.MustCompile(`(?is)<(?:atom:)?link\b[^>]*>`)
	attrRe    = regexp.MustCompile(`(?is)\b(rel|href)\s*=\s*["']([^"']*)["']`)
	linkHdrRe = regexp.MustCompile(`<([^>]+)>\s*;([^,]*)`)
	relRe     = regexp.MustCompile(`(?i)rel\s*=\s*"?([^";]+)"?`)
)

// bodyLinks finds <link rel="hub"> and <link rel="self"> in an XML feed.
func bodyLinks(body []byte) (hub, self string) {
	head := body
	if len(head) > 64<<10 {
		head = head[:64<<10]
	}
	for _, tag := range linkTagRe.FindAll(head, -1) {
		var rel, href string
		for _, m := range attrRe.FindAllSubmatch(tag, -1) {
			switch strings.ToLower(string(m[1])) {
			case "rel":
				rel = strings.ToLower(string(m[2]))
			case "href":
				href = html.UnescapeString(string(m[2]))
			}
		}
		switch {
		case rel == "hub" && hub == "":
			hub = href
		case rel == "self" && self == "":
			self = href
		}
	}
	return hub, self
}

// linkHeader reads WebSub links from HTTP Link headers.
func linkHeader(values []string) (hub, self string) {
	for _, v := range values {
		for _, m := range linkHdrRe.FindAllStringSubmatch(v, -1) {
			r := relRe.FindStringSubmatch(m[2])
			if r == nil {
				continue
			}
			for _, rel := range strings.Fields(strings.ToLower(r[1])) {
				if rel == "hub" && hub == "" {
					hub = m[1]
				}
				if rel == "self" && self == "" {
					self = m[1]
				}
			}
		}
	}
	return hub, self
}

var (
	tagRe   = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>|<[^>]*>`)
	spaceRe = regexp.MustCompile(`\s+`)
)

// clean turns HTML into one line of plain text.
func clean(s string) string {
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

// truncate cuts s to at most n bytes on a rune boundary, adding "…".
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return ""
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " ") + "…"
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
