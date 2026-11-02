# Hibira feed relay

Turn any RSS, Atom or JSON feed, including every [RSSHub](https://github.com/DIYgod/RSSHub) route, into realtime events. When a feed gets a new item, `feed-relay` sends it to [Hibira](https://hibira.io) as:

- a **channel event** that every open browser, dashboard and app receives at once (pusher-js, Laravel Echo or any Pusher client),
- a **push notification** to iOS, Android and the web (Hibira Push),
- an **inbox notification** for the [`@hibira-io/inbox`](https://github.com/hibira-io/inbox) bell.

It is one small Go binary or Docker image that runs next to your RSSHub or on any server. Feeds that announce a [WebSub](https://www.w3.org/TR/websub/) hub (YouTube channels, many blogs) are pushed to the relay within seconds; the rest are polled.

```
RSSHub / any feed ──poll or WebSub──▶ feed-relay ──HTTP API──▶ Hibira ──WebSocket / push──▶ browsers, apps
```

## Quick start

1. Create an app in the [Hibira portal](https://hibira.io/portal) and copy its ID, key and secret into `.env`:

   ```sh
   HIBIRA_APP_ID=123
   HIBIRA_APP_KEY=your-key
   HIBIRA_APP_SECRET=your-secret
   ```

2. Copy [`examples/feed-relay.yaml`](examples/feed-relay.yaml) and list your feeds:

   ```yaml
   feeds:
     - name: pusher-server releases
       url: http://rsshub:1200/github/release/hibira-io/pusher-server
       channel: releases
   ```

3. Run it with RSSHub ([`examples/docker-compose.yml`](examples/docker-compose.yml)):

   ```sh
   docker compose up -d
   ```

   or on its own:

   ```sh
   docker run --env-file .env -v $PWD/feed-relay.yaml:/config/feed-relay.yaml:ro \
     -v relay-data:/data -p 8080:8080 ghcr.io/hibira-io/feed-relay:latest
   ```

   Binaries for Linux, macOS and Windows are on the [releases page](https://github.com/hibira-io/feed-relay/releases).

4. Listen in the browser ([`examples/listen.html`](examples/listen.html)):

   ```js
   pusher.subscribe('releases').bind('new-item', (item) => {
     console.log(item.feed.title, item.title, item.link);
   });
   ```

## What a channel receives

Event `new-item` (or the feed's `event`), with:

```json
{
  "feed": { "name": "pusher-server releases", "title": "pusher-server releases", "link": "https://github.com/..." },
  "id": "tag:github.com,2008:Repository/1/v0.2.40",
  "title": "v0.2.40",
  "link": "https://github.com/hibira-io/pusher-server/releases/tag/v0.2.40",
  "summary": "Plain text, HTML removed",
  "author": "deflinhec",
  "image": "https://...",
  "published": "2026-11-02T08:00:00Z"
}
```

Each event is kept under `max_bytes` (default 3,500, under the Free plan's 4 KB): the summary is shortened first, then the title. Fetch the full article from `link` when you need it.

## Configuration

| Key | Default | |
|---|---|---|
| `hibira.url` | `https://ws.hibira.io` | `http://localhost:6001` for a local Pusher-compatible server |
| `hibira.app_id`, `key`, `secret` | `HIBIRA_APP_ID`, `HIBIRA_APP_KEY`, `HIBIRA_APP_SECRET` | required |
| `hibira.push_instance_id`, `push_secret_key` | `HIBIRA_PUSH_INSTANCE_ID`, `HIBIRA_PUSH_SECRET_KEY` | only for `push:` |
| `public_url` | `PUBLIC_URL` | a public https base URL; turns on WebSub |
| `listen` | `:8080` | `/healthz` and `/websub/...` |
| `state_file` | `feed-relay-state.json` | which items were sent; keep it on a volume |
| `defaults.interval` | `5m` | at least `30s` |
| `defaults.event` | `new-item` | |
| `defaults.max_bytes` | `3500` | |
| `defaults.max_items` | `20` | per fetch; a burst sends only the newest |

Per feed:

| Key | |
|---|---|
| `url` | the feed (required) |
| `name` | a unique name (default: the URL); used in events and the state file |
| `channel` / `channels` | where events go (up to 100) |
| `event` | event name |
| `interval` | overrides the default |
| `backfill` | items to send on the very first run (default 0: existing items are only remembered) |
| `headers` | extra request headers, for example an access key for a private RSSHub |
| `websub` | `false` to poll only |
| `push` | `interests` and/or `users`, optional `title` (default: the feed's title) |
| `inbox` | `users` (1 to 1,000), optional `category` and `push` (`offline`, `always`, `never`) |

## How it behaves

- **First run:** items already in the feed are remembered, not sent (unless `backfill`), so starting the relay never floods your users.
- **No duplicates:** sent item IDs (GUID, else link) are saved in `state_file`, so restarts do not resend. Inbox notifications also carry an idempotency key.
- **Polite fetching:** `ETag` and `Last-Modified` are sent back, so unchanged feeds answer `304`. Failing feeds back off up to 6× their interval.
- **Retries:** network errors, `429` and `5xx` from Hibira are retried, then the item waits for the next fetch. An item Hibira refuses for good (for example `413`) is logged and skipped.
- **WebSub:** with `public_url` set, the relay subscribes to the feed's hub (7-day lease, renewed a day before it ends), checks every push against its HMAC signature and keeps polling as a safety net.
- **Health:** `GET /healthz` lists each feed's last fetch, last send, count and last error.

Flags: `-config` (or `CONFIG`), `-once` (fetch every feed once and exit, for cron), `-check` (validate the config), `-json` (JSON logs), `-version`.

## Notes

- Only relay feeds you are allowed to use, and follow each source site's terms. Keep intervals reasonable.
- The relay sends events through the app's secret, so run it on your server, never in a browser.
- On the Free plan, list your page's origin in the app's allowed origins.

## Development

```sh
go test -race ./...
go run ./cmd/feed-relay -config examples/feed-relay.yaml
```

## 中文摘要

`feed-relay` 把任何 RSS／Atom／JSON Feed（包含所有 RSSHub 路由）的新條目，即時轉成 Hibira 的 channel 事件、App 推播或通知收件匣。單一 Go 執行檔或 Docker 映像，跟 RSSHub 放在一起跑即可。首次啟動只記住既有條目、不會洗版；已送出的條目記在 state 檔，重啟也不會重送；支援 ETag／304 與 WebSub（設定 `public_url` 後，YouTube 等有 hub 的 feed 可秒級送達）。事件預設控制在 3,500 bytes 內，符合 Free 方案 4 KB 上限。請只轉發你有權使用的 feed，並遵守來源網站條款。

## License

MIT
