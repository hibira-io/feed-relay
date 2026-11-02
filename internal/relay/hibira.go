package relay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Hibira sends events, push notifications and inbox notifications through
// the Hibira HTTP API (the Pusher Channels and Beams protocols).
type Hibira struct {
	cfg  HibiraConfig
	http *http.Client
	now  func() time.Time
}

func NewHibira(cfg HibiraConfig, client *http.Client) *Hibira {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Hibira{cfg: cfg, http: client, now: time.Now}
}

// APIError is a non-2xx answer. Retry tells whether trying again may help.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("hibira answered %d: %s", e.Status, strings.TrimSpace(e.Body))
}

func (e *APIError) Retry() bool { return e.Status == 429 || e.Status >= 500 }

// Trigger publishes one event to up to 100 channels.
func (h *Hibira) Trigger(ctx context.Context, channels []string, event string, data string) error {
	body, _ := json.Marshal(map[string]any{"name": event, "channels": channels, "data": data})
	return h.signed(ctx, "/apps/"+h.cfg.AppID+"/events", body)
}

// Inbox stores a notification in each user's inbox.
func (h *Hibira) Inbox(ctx context.Context, n map[string]any) error {
	body, _ := json.Marshal(n)
	return h.signed(ctx, "/apps/"+h.cfg.AppID+"/inbox/notifications", body)
}

// Push sends a push notification to interests or users (Beams publish API).
func (h *Hibira) Push(ctx context.Context, to string, list []string, title, body, link string) error {
	web := map[string]any{"title": title, "body": body}
	data := map[string]any{}
	if link != "" {
		web["deep_link"] = link
		data["url"] = link
	}
	payload := map[string]any{
		to:     list,
		"web":  map[string]any{"notification": web, "data": data},
		"fcm":  map[string]any{"notification": map[string]any{"title": title, "body": body}, "data": data},
		"apns": map[string]any{"aps": map[string]any{"alert": map[string]any{"title": title, "body": body}}, "data": data},
	}
	raw, _ := json.Marshal(payload)
	u := h.cfg.URL + "/publish_api/v1/instances/" + url.PathEscape(h.cfg.PushInstanceID) + "/publishes/" + to
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.cfg.PushSecretKey)
	return h.do(req)
}

// signed POSTs body to path with the Pusher HTTP API signature.
func (h *Hibira) signed(ctx context.Context, path string, body []byte) error {
	sum := md5.Sum(body)
	q := url.Values{}
	q.Set("auth_key", h.cfg.Key)
	q.Set("auth_timestamp", strconv.FormatInt(h.now().Unix(), 10))
	q.Set("auth_version", "1.0")
	q.Set("body_md5", hex.EncodeToString(sum[:]))
	q.Set("auth_signature", Sign(h.cfg.Secret, http.MethodPost, path, q))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.URL+path+"?"+q.Encode(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return h.do(req)
}

func (h *Hibira) do(req *http.Request) error {
	req.Header.Set("User-Agent", "hibira-feed-relay/"+Version)
	res, err := h.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode/100 != 2 {
		return &APIError{Status: res.StatusCode, Body: string(b)}
	}
	return nil
}

// Sign is the Pusher HTTP API signature: HMAC-SHA256 of
// "METHOD\nPATH\nsorted query" without auth_signature.
func Sign(secret, method, path string, q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		if k != "auth_signature" {
			keys = append(keys, strings.ToLower(k))
		}
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + q.Get(k)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(method + "\n" + path + "\n" + strings.Join(parts, "&")))
	return hex.EncodeToString(mac.Sum(nil))
}
