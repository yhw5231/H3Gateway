// Package server exposes the gateway over HTTP: the OpenAI-compatible /v1
// surface, the bundled studio UI and the back-office console.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yhw5231/H3Gateway/internal/auth"
	"github.com/yhw5231/H3Gateway/internal/config"
	"github.com/yhw5231/H3Gateway/internal/identity"
	"github.com/yhw5231/H3Gateway/internal/model"
	"github.com/yhw5231/H3Gateway/internal/pipeline"
	"github.com/yhw5231/H3Gateway/internal/store"
	"github.com/yhw5231/H3Gateway/internal/upstream"
	"github.com/yhw5231/H3Gateway/internal/xffprobe"
)

// Cookie names. The studio cookie keeps the bundled front end working without
// asking a human for a key; the admin cookie is the back office session.
const (
	studioCookie = "h3_studio"
	adminCookie  = "h3_admin"
)

// Session lifetimes.
const (
	studioTTL = 30 * 24 * time.Hour
	adminTTL  = 12 * time.Hour
)

// Server bundles the dependencies every handler needs.
type Server struct {
	env     config.Env
	store   *store.Store
	pipe    *pipeline.Pipeline
	up      *upstream.Client
	rot     *identity.Rotator
	log     *slog.Logger
	webFS   fs.FS
	mux     *http.ServeMux
	probeMu chan struct{}

	imageClientOnce sync.Once
	imageHTTP       *http.Client
}

// New builds the HTTP layer.
func New(env config.Env, st *store.Store, pipe *pipeline.Pipeline, up *upstream.Client,
	rot *identity.Rotator, log *slog.Logger, webFS fs.FS) *Server {

	s := &Server{
		env:     env,
		store:   st,
		pipe:    pipe,
		up:      up,
		rot:     rot,
		log:     log,
		webFS:   webFS,
		mux:     http.NewServeMux(),
		probeMu: make(chan struct{}, 1),
	}
	s.routes()
	return s
}

// Handler returns the root HTTP handler with logging and recovery applied.
func (s *Server) Handler() http.Handler {
	return s.recoverer(s.requestLogger(s.mux))
}

// SetVersion records the build version reported by the console.
func (s *Server) SetVersion(v string) {
	if v != "" {
		Version = v
	}
}

// ---------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------

func (s *Server) routes() {
	m := s.mux

	m.HandleFunc("GET /health", s.handleHealth)
	m.HandleFunc("GET /healthz", s.handleHealth)

	// OpenAI-compatible surface.
	m.HandleFunc("GET /v1/models", s.apiAuth(s.handleModels))
	m.HandleFunc("GET /v1/trial/usage", s.apiAuth(s.handleTrialUsage))
	m.HandleFunc("POST /v1/videos", s.apiAuth(s.handleCreateVideo))
	m.HandleFunc("GET /v1/videos/{id}", s.apiAuth(s.handleGetVideo))
	m.HandleFunc("GET /v1/videos/{id}/content", s.apiAuth(s.handleVideoContent))
	m.HandleFunc("POST /v1/chat/completions", s.apiAuth(s.handleChatCompletions))

	// Back office.
	m.HandleFunc("GET /admin", s.handleAdminPage)
	m.HandleFunc("GET /admin/", s.handleAdminPage)
	m.HandleFunc("GET /admin/{asset}", s.handleAdminAsset)
	m.HandleFunc("POST /admin/api/login", s.handleAdminLogin)
	m.HandleFunc("POST /admin/api/logout", s.handleAdminLogout)
	m.HandleFunc("GET /admin/api/session", s.adminAuth(s.handleAdminSession))
	m.HandleFunc("POST /admin/api/password", s.adminAuth(s.handleAdminPassword))
	m.HandleFunc("GET /admin/api/stats", s.adminAuth(s.handleAdminStats))
	m.HandleFunc("GET /admin/api/tasks", s.adminAuth(s.handleAdminTasks))
	m.HandleFunc("GET /admin/api/tasks/{id}", s.adminAuth(s.handleAdminTaskGet))
	m.HandleFunc("DELETE /admin/api/tasks/{id}", s.adminAuth(s.handleAdminTaskDelete))
	m.HandleFunc("POST /admin/api/tasks/{id}/retry", s.adminAuth(s.handleAdminTaskRetry))
	m.HandleFunc("GET /admin/api/keys", s.adminAuth(s.handleAdminKeys))
	m.HandleFunc("POST /admin/api/keys", s.adminAuth(s.handleAdminKeyCreate))
	m.HandleFunc("GET /admin/api/keys/{id}/secret", s.adminAuth(s.handleAdminKeyReveal))
	m.HandleFunc("PATCH /admin/api/keys/{id}", s.adminAuth(s.handleAdminKeyUpdate))
	m.HandleFunc("DELETE /admin/api/keys/{id}", s.adminAuth(s.handleAdminKeyDelete))
	m.HandleFunc("GET /admin/api/settings", s.adminAuth(s.handleAdminSettingsGet))
	m.HandleFunc("PUT /admin/api/settings", s.adminAuth(s.handleAdminSettingsPut))
	m.HandleFunc("GET /admin/api/probe", s.adminAuth(s.handleAdminProbeGet))
	m.HandleFunc("POST /admin/api/probe", s.adminAuth(s.handleAdminProbeRun))
	m.HandleFunc("GET /admin/api/upstream", s.adminAuth(s.handleAdminUpstream))
	m.HandleFunc("POST /admin/api/rotator/reset", s.adminAuth(s.handleAdminRotatorReset))

	// Studio UI (the catch-all must be registered last).
	m.HandleFunc("GET /", s.handleStudioIndex)
	m.HandleFunc("GET /studio/{path...}", s.handleStudioAsset)
}

// ---------------------------------------------------------------------------
// middleware
// ---------------------------------------------------------------------------

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic serving request", "path", r.URL.Path, "panic", rec)
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": map[string]any{"message": "internal error", "type": "server_error"},
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		// Health checks are noisy and uninteresting.
		if r.URL.Path == "/health" || r.URL.Path == "/healthz" {
			return
		}
		s.log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"bytes", sw.bytes,
			"dur", time.Since(start).Round(time.Millisecond).String(),
			"client", s.clientIP(r),
		)
	})
}

// clientIP resolves the caller address, honouring X-Forwarded-For only when the
// operator declared a trusted proxy in front of the gateway.
func (s *Server) clientIP(r *http.Request) string {
	if s.store.Settings().TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, ok := strings.Cut(xff, ","); ok {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(xff)
		}
		if rip := r.Header.Get("X-Real-IP"); rip != "" {
			return strings.TrimSpace(rip)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// authentication
// ---------------------------------------------------------------------------

type ctxKey string

const (
	ctxKeyAPIKey ctxKey = "api_key"
	ctxKeySource ctxKey = "source"
)

// bearerToken extracts the Authorization bearer token.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// apiAuth guards /v1/*. A request is accepted when it carries a valid API key, a
// studio session, an admin session, or when the gateway is deliberately open
// (no keys configured and require_api_key off — the behaviour of the original
// gateway when GATEWAY_API_KEY was unset).
func (s *Server) apiAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := s.store.Settings()
		secret := s.store.Secret()

		if token := bearerToken(r); token != "" {
			if k := s.store.FindKey(token); k != nil {
				s.store.TouchKey(k.ID)
				ctx := context.WithValue(r.Context(), ctxKeyAPIKey, k)
				next(w, r.WithContext(context.WithValue(ctx, ctxKeySource, "api")))
				return
			}
			// An explicitly supplied but invalid key must never fall through to
			// the anonymous path.
			writeOpenAIError(w, http.StatusUnauthorized, "invalid gateway API key", "invalid_api_key")
			return
		}

		if settings.StudioSessionEnabled {
			if c, err := r.Cookie(studioCookie); err == nil {
				if sess, err := auth.VerifySession(secret, c.Value); err == nil && sess.Scope == "studio" {
					ctx := context.WithValue(r.Context(), ctxKeySource, "studio")
					next(w, r.WithContext(ctx))
					return
				}
			}
		}
		if c, err := r.Cookie(adminCookie); err == nil {
			if sess, err := auth.VerifySession(secret, c.Value); err == nil && sess.Scope == "admin" {
				ctx := context.WithValue(r.Context(), ctxKeySource, "admin")
				next(w, r.WithContext(ctx))
				return
			}
		}

		if !settings.RequireAPIKey && !s.store.HasEnabledKeys() {
			next(w, r.WithContext(context.WithValue(r.Context(), ctxKeySource, "anonymous")))
			return
		}
		writeOpenAIError(w, http.StatusUnauthorized, "invalid gateway API key", "invalid_api_key")
	}
}

// adminAuth guards the back office. It requires a valid admin session plus, for
// state-changing verbs, a same-origin marker header to blunt CSRF.
func (s *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(adminCookie)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "not signed in"})
			return
		}
		sess, err := auth.VerifySession(s.store.Secret(), c.Value)
		if err != nil || sess.Scope != "admin" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "session expired"})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Requested-With") == "" {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "missing X-Requested-With header"})
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && !s.sameOrigin(r, origin) {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request rejected"})
				return
			}
		}
		// A signed session is only meaningful while its account still exists, so
		// deleting the account revokes outstanding tokens immediately.
		u, ok := s.store.GetUser(sess.Subject)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "account no longer exists"})
			return
		}
		if u.MustChangePassword && r.URL.Path != "/admin/api/password" && r.URL.Path != "/admin/api/session" {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error":                "password_change_required",
				"message":              "默认密码尚未修改，请先设置新密码",
				"must_change_password": true,
			})
			return
		}
		next(w, r)
	}
}

func (s *Server) sameOrigin(r *http.Request, origin string) bool {
	host := r.Host
	if s.store.Settings().TrustProxy {
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			host = h
		}
	}
	for _, scheme := range []string{"http://", "https://"} {
		if origin == scheme+host {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// response helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
			"code":    code,
		},
	})
}

// baseURL reconstructs the externally visible origin for generated links.
func (s *Server) baseURL(r *http.Request) string {
	settings := s.store.Settings()
	if settings.PublicBaseURL != "" {
		return settings.PublicBaseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if settings.TrustProxy {
		if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
			scheme = strings.TrimSpace(strings.Split(p, ",")[0])
		}
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			host = strings.TrimSpace(strings.Split(h, ",")[0])
		}
	}
	return scheme + "://" + host
}

// readJSON decodes a request body with a sane cap. Unknown fields are tolerated
// so OpenAI clients can keep sending parameters the gateway does not model.
func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// contextWithTimeout derives a bounded context from the request.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// ---------------------------------------------------------------------------
// shared handlers
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": "minimax-h3", "object": "model", "created": 1, "owned_by": "siftq-trial",
				"description": "MiniMax H3 图生视频，6 秒（匿名试用通道）"},
			{"id": "minimax-h3-10s", "object": "model", "created": 1, "owned_by": "siftq-trial",
				"description": "MiniMax H3 图生视频，10 秒（匿名试用通道）"},
			{"id": "minimax-h3-15s", "object": "model", "created": 1, "owned_by": "siftq-trial",
				"description": "MiniMax H3 图生视频，15 秒（匿名试用通道）"},
		},
	})
}

func (s *Server) handleTrialUsage(w http.ResponseWriter, r *http.Request) {
	// This endpoint is reachable by every API client, so it reports only what a
	// caller needs to size its own request: capacity and the active mode.
	// Operational counters stay behind /admin/api/stats.
	settings := s.store.Settings()
	probe := s.store.Probe()
	stats := s.pipe.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":            true,
		"in_flight":          stats.InFlight,
		"max_concurrent":     stats.MaxConcurrent,
		"xff_mode":           settings.XFFMode,
		"effective_xff_mode": identity.EffectiveMode(settings, probe),
		"ipv6_supported":     probe != nil && probe.OK && probe.IPv6Accepted,
		"endpoint_mode":      settings.EndpointMode,
	})
}

// ---------------------------------------------------------------------------
// static assets
// ---------------------------------------------------------------------------

func (s *Server) handleStudioIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.serveStudioFile(w, r, "index.html")
}

func (s *Server) handleStudioAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/studio/")
	if name == "" {
		http.NotFound(w, r)
		return
	}
	s.serveStudioFile(w, r, name)
}

func (s *Server) serveStudioFile(w http.ResponseWriter, r *http.Request, name string) {
	if s.webFS == nil {
		http.Error(w, "studio UI is not bundled", http.StatusNotFound)
		return
	}
	clean := strings.TrimPrefix(name, "/")
	if strings.Contains(clean, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(s.webFS, "web/studio/"+clean)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(clean, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(clean, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(clean, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(clean, ".jpg"), strings.HasSuffix(clean, ".jpeg"):
		w.Header().Set("Content-Type", "image/jpeg")
	case strings.HasSuffix(clean, ".png"):
		w.Header().Set("Content-Type", "image/png")
	}
	if clean == "index.html" && s.store.Settings().StudioSessionEnabled {
		// Mint the studio cookie so the bundled front end can call /v1/* without
		// asking a human for a key.
		http.SetCookie(w, &http.Cookie{
			Name:     studioCookie,
			Value:    auth.SignSession(s.store.Secret(), "studio", "", studioTTL),
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(studioTTL / time.Second),
		})
	}
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------------------
// helpers shared by handlers
// ---------------------------------------------------------------------------

func taskToVideoObject(t *model.Task, base string) map[string]any {
	obj := map[string]any{
		"id":             t.ID,
		"object":         "video",
		"model":          t.Model,
		"status":         t.Status,
		"progress":       progressOf(t),
		"created":        t.CreatedAt.Unix(),
		"completed_at":   nil,
		"ratio":          t.Ratio,
		"size":           t.Ratio,
		"seconds":        fmt.Sprintf("%d", t.Duration),
		"failure_reason": nil,
		"attempts":       t.Attempts,
		"created_at":     t.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":     t.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if t.Status == model.StatusSucceeded {
		url := base + "/v1/videos/" + t.ID + "/content"
		obj["video_url"] = url
		// "url" is the field OpenAI-style video clients read.
		obj["url"] = url
		obj["completed_at"] = t.UpdatedAt.Unix()
	}
	if t.Error != "" {
		obj["failure_reason"] = t.Error
	}
	if t.VideoSHA256 != "" {
		obj["video_sha256"] = t.VideoSHA256
	}
	if t.DuplicateOf != "" {
		// Upstream answered this task with content it had already produced. The
		// video is not this request's own render; say so instead of letting the
		// caller discover it by eye.
		obj["duplicate_of"] = t.DuplicateOf
	}
	return obj
}

func progressOf(t *model.Task) int {
	switch t.Status {
	case model.StatusSucceeded:
		return 100
	case model.StatusRunning:
		return 50
	case model.StatusQueued:
		return 10
	default:
		return 0
	}
}

// sanitizeFilename keeps a user-supplied filename safe for a Content-Disposition
// header.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\"", "")
	name = strings.ReplaceAll(name, "\r", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	if name == "" {
		name = "video.mp4"
	}
	return name
}

// probeRunning reports whether a capability probe currently holds the lock.
func (s *Server) tryLockProbe() bool {
	select {
	case s.probeMu <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) unlockProbe() { <-s.probeMu }

var errBadRequest = errors.New("bad request")

// runProbe executes the capability test and persists the outcome.
func (s *Server) runProbe(ctx context.Context, opts xffprobe.Options) *model.XFFProbeRecord {
	res := xffprobe.Run(ctx, s.up, s.store.Settings(), opts)
	rec := res.Record()
	s.store.SetProbe(rec)
	return rec
}
