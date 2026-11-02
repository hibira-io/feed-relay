package relay

import (
	"context"
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP server at /mcp (Streamable HTTP), on when mcp_token is set. AI
// agents use it to list, add, change, pause, check and test feeds; it works
// on the same feeds as the web console.

// FeedInput is a feed as an agent writes it.
type FeedInput struct {
	Name     string            `json:"name,omitempty" jsonschema:"unique name, shown in the console and sent as feed.name; defaults to the URL"`
	URL      string            `json:"url" jsonschema:"RSS, Atom or JSON Feed URL, or an RSSHub route such as /github/release/owner/repo when the relay has rsshub_url"`
	Channels []string          `json:"channels,omitempty" jsonschema:"Hibira channels that receive each new item as an event"`
	Event    string            `json:"event,omitempty" jsonschema:"event name; default new-item"`
	Interval string            `json:"interval,omitempty" jsonschema:"how often to check, such as 1m, 5m or 1h; at least 30s"`
	Backfill int               `json:"backfill,omitempty" jsonschema:"how many existing items to send when the feed is added; default 0 sends only items that appear later"`
	Headers  map[string]string `json:"headers,omitempty" jsonschema:"extra request headers, for example a key for a private RSSHub"`
	WebSub   *bool             `json:"websub,omitempty" jsonschema:"false to poll only even when the feed has a WebSub hub"`
	Push     *PushTarget       `json:"push,omitempty" jsonschema:"also send a push notification to these interests or users"`
	Inbox    *InboxTarget      `json:"inbox,omitempty" jsonschema:"also store a notification in these users' inboxes"`
	Paused   bool              `json:"paused,omitempty" jsonschema:"add or keep the feed paused"`
}

func (in FeedInput) feed() (Feed, error) {
	f := Feed{
		Name: in.Name, URL: in.URL, Channels: in.Channels, Event: in.Event, Backfill: in.Backfill,
		Headers: in.Headers, WebSub: in.WebSub, Push: in.Push, Inbox: in.Inbox, Disabled: in.Paused,
	}
	if err := f.Interval.parse(in.Interval); err != nil {
		return f, &ProblemsError{Problems: []string{err.Error()}}
	}
	return f, nil
}

type feedRef struct {
	Name string `json:"name" jsonschema:"the feed's name, as list_feeds shows it"`
}

type feedList struct {
	Feeds  []FeedInfo `json:"feeds"`
	Total  int        `json:"total"`
	Offset int        `json:"offset"`
}

type updateInput struct {
	Name string    `json:"name" jsonschema:"the feed to change, as list_feeds shows it"`
	Feed FeedInput `json:"feed" jsonschema:"the whole feed as it should be; to rename it, set a new feed.name"`
}

type pauseInput struct {
	Name   string `json:"name" jsonschema:"the feed's name"`
	Paused bool   `json:"paused" jsonschema:"true to pause, false to resume"`
}

type previewInput struct {
	URL     string            `json:"url" jsonschema:"feed URL or RSSHub route to read"`
	Headers map[string]string `json:"headers,omitempty" jsonschema:"extra request headers"`
	Limit   int               `json:"limit,omitempty" jsonschema:"newest items to return, default 5, at most 20"`
}

type findInput struct {
	URL string `json:"url" jsonschema:"a web page, such as https://github.com/vercel/next.js or a blog's home page"`
}

type routeSearch struct {
	Query  string `json:"query,omitempty" jsonschema:"site, namespace or words, such as github release or youtube channel; empty lists popular routes"`
	Lang   string `json:"lang,omitempty" jsonschema:"language for route names: en, ja, zh-CN or zh-TW"`
	Offset int    `json:"offset,omitempty" jsonschema:"skip this many results, for the next page"`
	Limit  int    `json:"limit,omitempty" jsonschema:"routes to return, default 20, at most 100"`
}

type listInput struct {
	Query  string `json:"query,omitempty" jsonschema:"only feeds whose name, URL, channel or event contains these words"`
	Status string `json:"status,omitempty" jsonschema:"only feeds that are running, paused or failing"`
	Offset int    `json:"offset,omitempty" jsonschema:"skip this many feeds, for the next page"`
	Limit  int    `json:"limit,omitempty" jsonschema:"feeds to return, default 50, at most 200"`
}

type checkResult struct {
	Sent int      `json:"sent"`
	Feed FeedInfo `json:"feed"`
}

type testResult struct {
	Item     Item     `json:"item"`
	Channels []string `json:"channels,omitempty"`
	Event    string   `json:"event"`
}

type exportInput struct {
	Format string `json:"format,omitempty" jsonschema:"json (default) for a full backup with every setting, or opml for a feed reader"`
}

type exportResult struct {
	Format string `json:"format"`
	Data   string `json:"data"`
}

type importInput struct {
	Data     string   `json:"data" jsonschema:"the text of a feed-relay JSON backup, a JSON list of feeds, or an OPML file"`
	Channels []string `json:"channels,omitempty" jsonschema:"channels for feeds that have none, such as every feed from OPML"`
	DryRun   bool     `json:"dry_run,omitempty" jsonschema:"true to only report what would be added and skipped"`
}

type deleted struct {
	Deleted string `json:"deleted"`
}

func (r *Relay) mcpServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "hibira-feed-relay", Title: "Hibira feed relay", Version: Version}, &mcp.ServerOptions{
		Instructions: "This relay watches RSS, Atom, JSON and RSSHub feeds and sends each new item to Hibira channels, push and inbox. " +
			"Use find_feeds or search_rsshub_routes to find a feed, and preview_feed to check it before add_feed. A new feed only remembers its current items; set backfill to also send some. " +
			"Feeds with source \"file\" come from the config file and cannot be changed here.",
	})
	yes, no := true, false
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &no}

	mcp.AddTool(s, &mcp.Tool{Name: "list_feeds", Description: "List feeds with their targets and status (last check, items sent, last error, WebSub state), optionally filtered by words or status. total is the number of matches; page with offset.", Annotations: readOnly},
		func(_ context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, any, error) {
			limit := in.Limit
			if limit <= 0 {
				limit = 50
			}
			feeds, total := r.FilterFeeds(in.Query, in.Status, in.Offset, min(limit, 200))
			return nil, feedList{Feeds: feeds, Total: total, Offset: max(in.Offset, 0)}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_feed", Description: "Get one feed and its status.", Annotations: readOnly},
		func(_ context.Context, _ *mcp.CallToolRequest, in feedRef) (*mcp.CallToolResult, any, error) {
			f, err := r.FindFeed(in.Name)
			if err != nil {
				return nil, nil, err
			}
			return nil, r.info(f), nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "preview_feed", Description: "Read a feed URL or RSSHub route without saving it: its title, item count, WebSub hub and newest items with the size of the event each would become.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &yes}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in previewInput) (*mcp.CallToolResult, any, error) {
			return result(r.PreviewFeed(ctx, in.URL, in.Headers, min(max(in.Limit, 0), 20)))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "find_feeds", Description: "Find the feeds a web page offers: the page itself if it is a feed, the feeds it links to, and the RSSHub routes that match it (when the relay has rsshub_url). Use the url of a result as add_feed's url.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &yes}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in findInput) (*mcp.CallToolResult, any, error) {
			return result(r.Discover(ctx, in.URL))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "search_rsshub_routes", Description: "Search the RSSHub instance's routes by site or topic. Each route lists its parameters; replace each :param in path with a value (drop optional ones you skip) and use the result as add_feed's url.",
		Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in routeSearch) (*mcp.CallToolResult, any, error) {
			limit := in.Limit
			if limit <= 0 {
				limit = 20
			}
			return result(r.SearchRoutes(ctx, in.Query, in.Lang, in.Offset, min(limit, 100)))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "add_feed", Description: "Add a feed and start watching it. Needs channels, push or inbox as a target.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: &yes, DestructiveHint: &no}},
		func(_ context.Context, _ *mcp.CallToolRequest, in FeedInput) (*mcp.CallToolResult, any, error) {
			f, err := in.feed()
			if err != nil {
				return nil, nil, err
			}
			return result(r.AddFeed(f, "mcp"))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "update_feed", Description: "Replace a feed's settings. Send the whole feed: fields left out go back to their defaults. Changing the URL starts over, remembering the new feed's current items.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: &yes, DestructiveHint: &yes, IdempotentHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, in updateInput) (*mcp.CallToolResult, any, error) {
			f, err := in.Feed.feed()
			if err != nil {
				return nil, nil, err
			}
			return result(r.UpdateFeed(in.Name, f, "mcp"))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "pause_feed", Description: "Pause or resume a feed.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: &no, OpenWorldHint: &no}},
		func(_ context.Context, _ *mcp.CallToolRequest, in pauseInput) (*mcp.CallToolResult, any, error) {
			return result(r.SetPaused(in.Name, in.Paused, "mcp"))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "delete_feed", Description: "Stop watching a feed and forget which items it sent.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes, IdempotentHint: true, OpenWorldHint: &no}},
		func(_ context.Context, _ *mcp.CallToolRequest, in feedRef) (*mcp.CallToolResult, any, error) {
			if err := r.DeleteFeed(in.Name, "mcp"); err != nil {
				return nil, nil, err
			}
			return nil, deleted{Deleted: in.Name}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "check_feed", Description: "Check a feed now and send its new items, instead of waiting for the next check.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: &yes, DestructiveHint: &no}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in feedRef) (*mcp.CallToolResult, any, error) {
			sent, info, err := r.CheckNow(ctx, in.Name)
			if err != nil {
				return nil, nil, err
			}
			return nil, checkResult{Sent: sent, Feed: info}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "send_test_item", Description: "Send the feed's newest item to its targets now, even if it was sent before, to check the wiring end to end.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: &yes, DestructiveHint: &no}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in feedRef) (*mcp.CallToolResult, any, error) {
			it, f, err := r.SendTest(ctx, in.Name)
			if err != nil {
				return nil, nil, err
			}
			return nil, testResult{Item: it, Channels: f.Channels, Event: f.Event}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "export_feeds", Description: "Export every feed: format json is a full backup that import_feeds restores (it includes request headers, which may hold keys); opml lists names and URLs for a feed reader.", Annotations: readOnly},
		func(_ context.Context, _ *mcp.CallToolRequest, in exportInput) (*mcp.CallToolResult, any, error) {
			if in.Format == "opml" {
				raw, err := r.ExportOPML()
				return result(exportResult{Format: "opml", Data: string(raw)}, err)
			}
			raw, err := r.ExportJSON()
			return result(exportResult{Format: "json", Data: string(raw)}, err)
		})
	mcp.AddTool(s, &mcp.Tool{Name: "import_feeds", Description: "Add the feeds of a JSON backup or an OPML file. Feeds whose name or URL is already watched, and feeds with problems, are skipped and listed. Imported feeds only remember their current items. Try dry_run first.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: &yes, DestructiveHint: &no}},
		func(_ context.Context, _ *mcp.CallToolRequest, in importInput) (*mcp.CallToolResult, any, error) {
			feeds, format, err := r.ParseImport([]byte(in.Data))
			if err != nil {
				return nil, nil, err
			}
			return result(r.ImportFeeds(feeds, in.Channels, format, in.DryRun, "mcp"))
		})
	return s
}

// result turns a value and error into a tool handler's return values.
func result[T any](v T, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return nil, nil, err
	}
	return nil, v, nil
}

// mcpHandler checks the bearer token, then serves MCP over Streamable HTTP.
func (r *Relay) mcpHandler() http.Handler {
	server := r.mcpServer()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:      true,
		JSONResponse:   true,
		Logger:         r.log,
		SessionTimeout: 30 * time.Minute,
		// The token already proves the caller; the relay usually runs in a
		// container reached by its service name, not "localhost".
		DisableLocalhostProtection: true,
	})
	want := []byte("Bearer " + r.cfg.MCPToken)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got := []byte(req.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="feed-relay"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Send Authorization: Bearer <MCP_TOKEN>."})
			return
		}
		h.ServeHTTP(w, req)
	})
}
