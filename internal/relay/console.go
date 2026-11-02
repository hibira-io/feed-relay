package relay

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The web console, on when admin_password is set: list, add, edit, pause
// and delete feeds, preview a feed before saving it and send a test item.
// Feeds added here are saved in feeds_file; feeds from the config file are
// shown read-only.

//go:embed console
var consoleFiles embed.FS

const (
	sessionCookie = "feed_relay_session"
	sessionTTL    = 7 * 24 * time.Hour
)

// consoleFeeds is the feeds_file: the feeds managed in the console.
type consoleFeeds struct {
	path string
	mu   sync.Mutex
}

// LoadConsoleFeeds reads feeds_file into the relay. A missing file is no
// feeds yet.
func (r *Relay) LoadConsoleFeeds() error {
	if r.cfg.AdminPassword == "" && r.cfg.MCPToken == "" {
		return nil
	}
	r.console = &consoleFeeds{path: r.cfg.FeedsFile}
	raw, err := os.ReadFile(r.cfg.FeedsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var feeds []Feed
	if err := json.Unmarshal(raw, &feeds); err != nil {
		return fmt.Errorf("%s: %w", r.cfg.FeedsFile, err)
	}
	for i := range feeds {
		f := feeds[i]
		f.Source = "console"
		if err := r.cfg.CheckFeed(&f); err != nil {
			r.log.Warn("console feed has a problem, kept paused", "feed", f.Name, "err", err)
			f.Disabled = true
		}
		if r.feed(f.Name) != nil {
			r.log.Warn("console feed has the name of a config file feed, skipped", "feed", f.Name)
			continue
		}
		r.put(f.Name, &f)
	}
	return nil
}

// saveConsoleFeeds writes every console feed to feeds_file atomically.
func (r *Relay) saveConsoleFeeds() error {
	r.console.mu.Lock()
	defer r.console.mu.Unlock()
	var feeds []Feed
	for _, f := range r.Feeds() {
		if f.Source == "console" {
			feeds = append(feeds, *f)
		}
	}
	if feeds == nil {
		feeds = []Feed{}
	}
	raw, err := json.MarshalIndent(feeds, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.console.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".feeds-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), r.console.path)
}

func (r *Relay) mountConsole(mux *http.ServeMux) {
	static, _ := fs.Sub(consoleFiles, "console")
	files := http.FileServerFS(static)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' "+r.wsOrigin()+"; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeFileFS(w, req, static, "index.html")
	})
	mux.Handle("GET /console/", http.StripPrefix("/console/", files))
	mux.HandleFunc("POST /api/login", r.apiLogin)
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/state", r.authed(r.apiState))
	mux.HandleFunc("POST /api/feeds", r.authed(r.apiCreate))
	mux.HandleFunc("POST /api/feeds/{id}", r.authed(r.apiUpdate))
	mux.HandleFunc("DELETE /api/feeds/{id}", r.authed(r.apiDelete))
	mux.HandleFunc("POST /api/feeds/{id}/pause", r.authed(r.apiPause))
	mux.HandleFunc("POST /api/feeds/{id}/poll", r.authed(r.apiPoll))
	mux.HandleFunc("POST /api/feeds/{id}/test", r.authed(r.apiTest))
	mux.HandleFunc("POST /api/preview", r.authed(r.apiPreview))
	mux.HandleFunc("POST /api/live/auth", r.authed(r.apiLiveAuth))
}

// --- sessions ---

func (r *Relay) sessionValue(issued int64) string {
	mac := hmac.New(sha256.New, []byte(r.cfg.AdminPassword))
	fmt.Fprintf(mac, "feed-relay session\x00%d", issued)
	return strconv.FormatInt(issued, 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (r *Relay) validSession(req *http.Request) bool {
	c, err := req.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	ts, _, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	issued, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(issued, 0)) > sessionTTL || time.Until(time.Unix(issued, 0)) > time.Minute {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(r.sessionValue(issued)))
}

// authed checks the session, and for changes the X-Feed-Relay header, which
// a cross-site form cannot send.
func (r *Relay) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if !r.validSession(req) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Sign in again."})
			return
		}
		if req.Method != http.MethodGet && req.Header.Get("X-Feed-Relay") != "1" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Missing X-Feed-Relay header."})
			return
		}
		h(w, req)
	}
}

var loginMu sync.Mutex

func (r *Relay) apiLogin(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(req.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Send the password."})
		return
	}
	// one attempt at a time, each at least half a second: slows guessing
	loginMu.Lock()
	defer loginMu.Unlock()
	time.Sleep(500 * time.Millisecond)
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(r.cfg.AdminPassword)) != 1 {
		r.log.Warn("console sign-in failed", "remote", req.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "That password is not right."})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: r.sessionValue(time.Now().Unix()), Path: "/",
		MaxAge: int(sessionTTL / time.Second), HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: req.TLS != nil || req.Header.Get("X-Forwarded-Proto") == "https",
	})
	w.WriteHeader(http.StatusNoContent)
}

// --- feeds ---

func (r *Relay) apiState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version": Version,
		"hibira": map[string]any{
			"url":    r.cfg.Hibira.URL,
			"app_id": r.cfg.Hibira.AppID,
			"push":   r.cfg.Hibira.PushInstanceID != "" && r.cfg.Hibira.PushSecretKey != "",
		},
		"rsshub_url": r.cfg.RSSHubURL,
		"websub":     r.cfg.PublicURL != "",
		"mcp":        r.cfg.MCPToken != "",
		"live":       r.liveConfig(),
		"defaults": map[string]any{
			"interval": r.cfg.Defaults.Interval,
			"event":    r.cfg.Defaults.Event,
		},
		"feeds": r.FeedInfos(),
	})
}

func readJSON(w http.ResponseWriter, req *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(req.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "That request is not valid: " + err.Error()})
		return false
	}
	return true
}

// answer writes v, or the error with a fitting status.
func answer(w http.ResponseWriter, status int, v any, err error) {
	var pe *ProblemsError
	switch {
	case err == nil:
		writeJSON(w, status, v)
	case errors.As(err, &pe):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "Fix these first.", "problems": pe.Problems})
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "That feed is gone. Reload the page."})
	case errors.Is(err, ErrReadOnly):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "This feed is set in the config file; change it there."})
	case errors.Is(err, ErrExists):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "A feed with that name already exists."})
	case errors.Is(err, ErrNoItems):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "The feed has no items to send."})
	default:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": capitalize(err.Error())})
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (r *Relay) apiCreate(w http.ResponseWriter, req *http.Request) {
	var f Feed
	if !readJSON(w, req, &f) {
		return
	}
	info, err := r.AddFeed(f, "console")
	answer(w, http.StatusCreated, info, err)
}

func (r *Relay) apiUpdate(w http.ResponseWriter, req *http.Request) {
	var f Feed
	if !readJSON(w, req, &f) {
		return
	}
	info, err := r.UpdateFeed(req.PathValue("id"), f, "console")
	answer(w, http.StatusOK, info, err)
}

func (r *Relay) apiDelete(w http.ResponseWriter, req *http.Request) {
	if err := r.DeleteFeed(req.PathValue("id"), "console"); err != nil {
		answer(w, 0, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *Relay) apiPause(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Paused bool `json:"paused"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	info, err := r.SetPaused(req.PathValue("id"), body.Paused, "console")
	answer(w, http.StatusOK, info, err)
}

func (r *Relay) apiPoll(w http.ResponseWriter, req *http.Request) {
	sent, info, err := r.CheckNow(req.Context(), req.PathValue("id"))
	answer(w, http.StatusOK, map[string]any{"sent": sent, "status": info.Status}, err)
}

func (r *Relay) apiTest(w http.ResponseWriter, req *http.Request) {
	it, f, err := r.SendTest(req.Context(), req.PathValue("id"))
	if err != nil {
		answer(w, 0, nil, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": it, "channels": f.Channels, "event": f.Event})
}

func (r *Relay) apiPreview(w http.ResponseWriter, req *http.Request) {
	var body struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	p, err := r.PreviewFeed(req.Context(), body.URL, body.Headers, 5)
	answer(w, http.StatusOK, p, err)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
