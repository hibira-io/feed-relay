package relay

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Export and import. A JSON backup keeps every setting of every feed and
// restores it on another relay; OPML is the list format feed readers use,
// with only names and URLs.

// Backup is a JSON export.
type Backup struct {
	App      string `json:"app"`
	Version  int    `json:"version"`
	Exported string `json:"exported"`
	Feeds    []Feed `json:"feeds"`
}

const maxImport = 1000

var ErrImportFormat = errors.New("not a feed-relay backup or an OPML file")

// ExportJSON is every feed, from the config file and the console, with all
// its settings, request headers included.
func (r *Relay) ExportJSON() ([]byte, error) {
	b := Backup{App: "feed-relay", Version: 1, Exported: time.Now().UTC().Format(time.RFC3339), Feeds: []Feed{}}
	for _, f := range r.Feeds() {
		b.Feeds = append(b.Feeds, *f)
	}
	return json.MarshalIndent(b, "", "  ")
}

type opmlDoc struct {
	XMLName xml.Name    `xml:"opml"`
	Version string      `xml:"version,attr"`
	Title   string      `xml:"head>title"`
	Created string      `xml:"head>dateCreated,omitempty"`
	Body    []opmlEntry `xml:"body>outline"`
}

type opmlEntry struct {
	Type     string      `xml:"type,attr,omitempty"`
	Text     string      `xml:"text,attr"`
	Title    string      `xml:"title,attr,omitempty"`
	XMLURL   string      `xml:"xmlUrl,attr,omitempty"`
	HTMLURL  string      `xml:"htmlUrl,attr,omitempty"`
	Children []opmlEntry `xml:"outline"`
}

// ExportOPML lists every feed for a feed reader. RSSHub routes are written
// as full URLs on rsshub_url.
func (r *Relay) ExportOPML() ([]byte, error) {
	doc := opmlDoc{Version: "2.0", Title: "feed-relay feeds", Created: time.Now().UTC().Format(time.RFC1123Z)}
	for _, f := range r.Feeds() {
		u := f.FetchURL
		if u == "" {
			u = f.URL
		}
		doc.Body = append(doc.Body, opmlEntry{Type: "rss", Text: f.Name, Title: f.Name, XMLURL: u})
	}
	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(out, '\n')...), nil
}

// ParseImport reads a JSON backup (or a bare list of feeds) or an OPML
// file. Feeds from OPML have only a name and URL; a URL on rsshub_url turns
// back into a route.
func (r *Relay) ParseImport(data []byte) (feeds []Feed, format string, err error) {
	data = bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	switch {
	case len(data) == 0:
		return nil, "", ErrImportFormat
	case data[0] == '<':
		var doc opmlDoc
		if err := xml.Unmarshal(data, &doc); err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrImportFormat, err)
		}
		var walk func([]opmlEntry)
		walk = func(es []opmlEntry) {
			for _, e := range es {
				if u := strings.TrimSpace(e.XMLURL); u != "" {
					name := strings.TrimSpace(e.Title)
					if name == "" {
						name = strings.TrimSpace(e.Text)
					}
					if base := r.cfg.RSSHubURL; base != "" && strings.HasPrefix(u, base+"/") {
						u = strings.TrimPrefix(u, base)
					}
					feeds = append(feeds, Feed{Name: name, URL: u})
				}
				walk(e.Children)
			}
		}
		walk(doc.Body)
		format = "opml"
	case data[0] == '[':
		if err := json.Unmarshal(data, &feeds); err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrImportFormat, err)
		}
		format = "json"
	case data[0] == '{':
		var b Backup
		if err := json.Unmarshal(data, &b); err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrImportFormat, err)
		}
		if b.Feeds == nil {
			return nil, "", ErrImportFormat
		}
		feeds, format = b.Feeds, "json"
	default:
		return nil, "", ErrImportFormat
	}
	if len(feeds) > maxImport {
		return nil, "", &ProblemsError{Problems: []string{fmt.Sprintf("at most %d feeds at once; this file has %d", maxImport, len(feeds))}}
	}
	return feeds, format, nil
}

// ImportResult says what an import added and what it left out.
type ImportResult struct {
	Format  string       `json:"format"`
	DryRun  bool         `json:"dry_run"`
	Added   []ImportItem `json:"added"`
	Skipped []ImportItem `json:"skipped"`
}

// ImportItem is one feed of an import. Reason is why it was left out:
// "exists" (a feed has that name), "same_url" (a feed reads that URL, named
// in Other) or "invalid" (see Problems).
type ImportItem struct {
	Name     string   `json:"name"`
	URL      string   `json:"url"`
	Reason   string   `json:"reason,omitempty"`
	Other    string   `json:"other,omitempty"`
	Problems []string `json:"problems,omitempty"`
}

// ImportFeeds adds feeds as console feeds. Feeds without channels get
// channels. Like any new feed, each only remembers its current items: a
// backfill in the file is not repeated. With dryRun nothing changes.
func (r *Relay) ImportFeeds(feeds []Feed, channels []string, format string, dryRun bool, by string) (ImportResult, error) {
	res := ImportResult{Format: format, DryRun: dryRun, Added: []ImportItem{}, Skipped: []ImportItem{}}
	names, urls := map[string]bool{}, map[string]string{}
	for _, f := range r.Feeds() {
		names[f.Name] = true
		urls[f.FetchURL] = f.Name
	}
	var add []Feed
	for _, f := range feeds {
		f.Source = "console"
		f.Backfill = 0
		if len(f.Channels) == 0 && f.Channel == "" {
			f.Channels = channels
		}
		err := r.cfg.CheckFeed(&f)
		item := ImportItem{Name: f.Name, URL: f.URL}
		switch {
		case err != nil:
			item.Reason, item.Problems = "invalid", strings.Split(err.Error(), "\n")
		case names[f.Name]:
			item.Reason = "exists"
		case urls[f.FetchURL] != "":
			item.Reason, item.Other = "same_url", urls[f.FetchURL]
		}
		if item.Reason != "" {
			res.Skipped = append(res.Skipped, item)
			continue
		}
		names[f.Name], urls[f.FetchURL] = true, f.Name
		res.Added = append(res.Added, item)
		add = append(add, f)
	}
	if dryRun || len(add) == 0 {
		return res, nil
	}
	for i := range add {
		f := add[i]
		r.store.Delete(f.Name)
		r.put(f.Name, &f)
	}
	r.announce(by, "imported", fmt.Sprint(len(add)), "")
	return res, r.saveConsoleFeeds()
}
