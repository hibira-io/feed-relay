package relay

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// WebSub (W3C, formerly PubSubHubbub): feeds that name a hub, such as
// YouTube channels and many blogs, push new entries to the relay within
// seconds instead of waiting for the next poll. Polling keeps running as a
// safety net.

const (
	leaseSeconds = 7 * 24 * 3600
	renewBefore  = 24 * time.Hour
	retryAfter   = time.Hour
)

// renewWebSub subscribes when the feed has a hub and the lease is missing
// or ends within a day.
func (r *Relay) renewWebSub(ctx context.Context, f *Feed) {
	if !r.cfg.webSubOn(f) {
		return
	}
	st := r.store.Get(f.Name)
	if st.Hub == "" || time.Until(st.LeaseUntil) > renewBefore || time.Now().Before(st.RetryAt) {
		return
	}
	// whatever happens, do not ask again for an hour: the hub verifies
	// asynchronously and sets the lease through the callback
	r.store.Update(f.Name, func(s *FeedState) { s.RetryAt = time.Now().Add(retryAfter) })
	if err := r.subscribe(ctx, f, st.Hub, st.Topic, "subscribe"); err != nil {
		r.log.Warn("websub subscribe failed", "feed", f.Name, "hub", st.Hub, "err", err)
		r.setStatus(f.Name, func(s *feedStatus) { s.WebSub = "error: " + err.Error() })
		return
	}
	r.setStatus(f.Name, func(s *feedStatus) { s.WebSub = "pending" })
}

func (r *Relay) subscribe(ctx context.Context, f *Feed, hub, topic, mode string) error {
	form := url.Values{
		"hub.mode":          {mode},
		"hub.topic":         {topic},
		"hub.callback":      {r.cfg.PublicURL + "/websub/" + hookID(f.Name)},
		"hub.secret":        {r.hubSecret(f)},
		"hub.lease_seconds": {strconv.Itoa(leaseSeconds)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hub, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", r.cfg.Defaults.UserAgent)
	res, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("hub answered %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// hubSecret is derived from the app secret, so it survives restarts
// without being stored.
func (r *Relay) hubSecret(f *Feed) string {
	mac := hmac.New(sha256.New, []byte(r.cfg.Hibira.Secret))
	mac.Write([]byte("feed-relay websub\x00" + f.Name))
	return hex.EncodeToString(mac.Sum(nil))[:40]
}

func (r *Relay) webSubCallback(w http.ResponseWriter, req *http.Request) {
	f := r.feedByHook(req.PathValue("id"))
	if f == nil || !r.cfg.webSubOn(f) {
		http.NotFound(w, req)
		return
	}
	switch req.Method {
	case http.MethodGet:
		r.verifyIntent(w, req, f)
	case http.MethodPost:
		r.receive(w, req, f)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// verifyIntent answers the hub's check that we asked for the subscription.
func (r *Relay) verifyIntent(w http.ResponseWriter, req *http.Request, f *Feed) {
	q := req.URL.Query()
	st := r.store.Get(f.Name)
	mode, topic := q.Get("hub.mode"), q.Get("hub.topic")
	switch mode {
	case "denied":
		r.log.Warn("websub subscription denied", "feed", f.Name, "reason", q.Get("hub.reason"))
		r.setStatus(f.Name, func(s *feedStatus) { s.WebSub = "denied" })
		w.WriteHeader(http.StatusOK)
		return
	case "subscribe":
		if topic != st.Topic {
			http.NotFound(w, req)
			return
		}
		lease, _ := strconv.Atoi(q.Get("hub.lease_seconds"))
		if lease <= 0 {
			lease = leaseSeconds
		}
		r.store.Update(f.Name, func(s *FeedState) { s.LeaseUntil = time.Now().Add(time.Duration(lease) * time.Second) })
		_ = r.store.Save()
		r.setStatus(f.Name, func(s *feedStatus) { s.WebSub = "active" })
		r.log.Info("websub subscribed", "feed", f.Name, "lease", time.Duration(lease)*time.Second)
	case "unsubscribe":
		// we never unsubscribe on purpose; refuse so the subscription stays
		http.NotFound(w, req)
		return
	default:
		http.Error(w, "unknown hub.mode", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, q.Get("hub.challenge"))
}

// receive takes a content distribution: the new entries as a feed document.
func (r *Relay) receive(w http.ResponseWriter, req *http.Request, f *Feed) {
	body, err := io.ReadAll(io.LimitReader(req.Body, maxFeedBytes+1))
	if err != nil || len(body) > maxFeedBytes {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	// The spec says to acknowledge with 2xx even when the signature is
	// wrong, and then ignore the content.
	w.WriteHeader(http.StatusAccepted)
	if !validSignature(req.Header.Get("X-Hub-Signature"), r.hubSecret(f), body) {
		r.log.Warn("websub content with a bad signature ignored", "feed", f.Name)
		return
	}
	p, err := Parse(body)
	if err != nil {
		r.log.Warn("websub content is not a feed", "feed", f.Name, "err", err)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := r.process(ctx, f, p); err == nil {
			_ = r.store.Save()
		}
	}()
}

func validSignature(header, secret string, body []byte) bool {
	algo, sig, ok := strings.Cut(header, "=")
	if !ok {
		return false
	}
	var h func() hash.Hash
	switch strings.ToLower(algo) {
	case "sha1":
		h = sha1.New
	case "sha256":
		h = sha256.New
	case "sha384":
		h = sha512.New384
	case "sha512":
		h = sha512.New
	default:
		return false
	}
	want, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(h, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}
