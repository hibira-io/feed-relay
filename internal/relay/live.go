package relay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Live updates for the web console, sent through Hibira itself: every change
// to the feeds (from the console or an MCP agent) is published to a private
// channel, and the console subscribes to it, refreshes and shows a toast.

// LiveChannel is the private channel the console listens on.
const LiveChannel = "private-feed-relay"

// LiveEvent is published on LiveChannel when a feed changes.
type LiveEvent struct {
	Action string `json:"action"`        // added, updated, deleted, paused, resumed
	Feed   string `json:"feed"`          // the feed's name
	Was    string `json:"was,omitempty"` // the old name, when renamed
	By     string `json:"by"`            // console or mcp
	At     string `json:"at"`
}

// announce publishes a change in the background; the change itself never
// waits on it or fails because of it.
func (r *Relay) announce(by, action, feed, was string) {
	if r.cfg.AdminPassword == "" {
		return // nobody is listening
	}
	if was == feed {
		was = ""
	}
	data, _ := json.Marshal(LiveEvent{Action: action, Feed: feed, Was: was, By: by, At: time.Now().UTC().Format(time.RFC3339)})
	r.liveOnce.Do(func() {
		r.liveQ = make(chan string, 64)
		go r.sendLive()
	})
	select {
	case r.liveQ <- string(data):
	default:
		r.log.Warn("live update dropped: too many at once")
	}
}

// sendLive publishes queued updates one by one, so they arrive in order.
func (r *Relay) sendLive() {
	for data := range r.liveQ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := r.hibira.Trigger(ctx, []string{LiveChannel}, "feeds.changed", data); err != nil {
			r.log.Warn("live update not sent", "err", err)
		}
		cancel()
	}
}

// liveConfig tells the console where to connect.
func (r *Relay) liveConfig() map[string]any {
	u, err := url.Parse(r.cfg.Hibira.URL)
	if err != nil {
		return nil
	}
	tls := u.Scheme == "https"
	port := u.Port()
	if port == "" {
		port = "80"
		if tls {
			port = "443"
		}
	}
	p, _ := strconv.Atoi(port)
	return map[string]any{"key": r.cfg.Hibira.Key, "host": u.Hostname(), "port": p, "tls": tls, "channel": LiveChannel}
}

// wsOrigin is the WebSocket origin the console's CSP must allow.
func (r *Relay) wsOrigin() string {
	u, err := url.Parse(r.cfg.Hibira.URL)
	if err != nil {
		return ""
	}
	scheme := "wss"
	if u.Scheme == "http" {
		scheme = "ws"
	}
	return scheme + "://" + u.Host
}

// apiLiveAuth signs the console's subscription to LiveChannel, and only that.
func (r *Relay) apiLiveAuth(w http.ResponseWriter, req *http.Request) {
	var body struct {
		SocketID string `json:"socket_id"`
		Channel  string `json:"channel_name"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	if body.Channel != LiveChannel || body.SocketID == "" || len(body.SocketID) > 64 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Only " + LiveChannel + " can be signed here."})
		return
	}
	mac := hmac.New(sha256.New, []byte(r.cfg.Hibira.Secret))
	mac.Write([]byte(body.SocketID + ":" + body.Channel))
	writeJSON(w, http.StatusOK, map[string]string{"auth": r.cfg.Hibira.Key + ":" + hex.EncodeToString(mac.Sum(nil))})
}
