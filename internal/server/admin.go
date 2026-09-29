package server

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yhw5231/H3Gateway/internal/auth"
	"github.com/yhw5231/H3Gateway/internal/config"
	"github.com/yhw5231/H3Gateway/internal/identity"
	"github.com/yhw5231/H3Gateway/internal/model"
	"github.com/yhw5231/H3Gateway/internal/store"
	"github.com/yhw5231/H3Gateway/internal/xffprobe"
)

// ---------------------------------------------------------------------------
// page
// ---------------------------------------------------------------------------

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	// Only the console itself is served here. "/admin/" is a subtree pattern, so
	// without this check every unknown path underneath it — a mistyped API call,
	// a missing asset — would be answered with the SPA shell and a 200.
	if r.URL.Path != "/admin" && r.URL.Path != "/admin/" {
		if strings.HasPrefix(r.URL.Path, "/admin/api/") {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "未知的后台接口: " + r.URL.Path})
			return
		}
		http.NotFound(w, r)
		return
	}
	if s.webFS == nil {
		http.Error(w, "admin UI is not bundled", http.StatusNotFound)
		return
	}
	data, err := fs.ReadFile(s.webFS, "web/admin/index.html")
	if err != nil {
		http.Error(w, "admin UI missing", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// handleAdminAsset serves the console's own static files. It is registered as a
// single-segment pattern so a missing file is a real 404 instead of the HTML
// shell — serving HTML where JavaScript is expected silently breaks the console.
func (s *Server) handleAdminAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("asset")
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		http.NotFound(w, r)
		return
	}
	if s.webFS == nil {
		http.Error(w, "admin UI is not bundled", http.StatusNotFound)
		return
	}
	data, err := fs.ReadFile(s.webFS, "web/admin/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", adminContentType(name))
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

func adminContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".ico"):
		return "image/x-icon"
	case strings.HasSuffix(name, ".json"):
		return "application/json; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// ---------------------------------------------------------------------------
// session
// ---------------------------------------------------------------------------

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	user, ok := s.store.GetUser(strings.TrimSpace(req.Username))
	if !ok || !auth.VerifyPassword(req.Password, user.PasswordHash, user.Salt, user.Iterations) {
		s.log.Warn("failed admin login", "user", req.Username, "client", s.clientIP(r))
		// A fixed delay blunts trivial password guessing against the bootstrap
		// credentials.
		time.Sleep(600 * time.Millisecond)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "用户名或密码错误"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookie,
		Value:    auth.SignSession(s.store.Secret(), "admin", user.Username, adminTTL),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(adminTTL / time.Second),
	})
	s.log.Info("admin signed in", "user", user.Username, "client", s.clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"username":             user.Username,
		"must_change_password": user.MustChangePassword,
	})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: adminCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminSession(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie(adminCookie)
	sess, err := auth.VerifySession(s.store.Secret(), c.Value)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "session expired"})
		return
	}
	user, _ := s.store.GetUser(sess.Subject)
	mustChange := false
	if user != nil {
		mustChange = user.MustChangePassword
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username":             sess.Subject,
		"must_change_password": mustChange,
		"expires_at":           sess.Expires.UTC().Format(time.RFC3339),
	})
}

type passwordRequest struct {
	Current string `json:"current_password"`
	New     string `json:"new_password"`
}

func (s *Server) handleAdminPassword(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	if len(req.New) < 8 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "新密码至少 8 位"})
		return
	}
	c, _ := r.Cookie(adminCookie)
	sess, err := auth.VerifySession(s.store.Secret(), c.Value)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "session expired"})
		return
	}
	user, ok := s.store.GetUser(sess.Subject)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "账号不存在"})
		return
	}
	if !auth.VerifyPassword(req.Current, user.PasswordHash, user.Salt, user.Iterations) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "当前密码不正确"})
		return
	}
	hash, salt, err := auth.HashPassword(req.New, auth.DefaultIterations)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	user.PasswordHash = hash
	user.Salt = salt
	user.Iterations = auth.DefaultIterations
	user.UpdatedAt = time.Now()
	user.MustChangePassword = false
	s.store.UpsertUser(user)

	// Re-issue the cookie so the caller is not logged out by the change.
	http.SetCookie(w, &http.Cookie{
		Name:  adminCookie,
		Value: auth.SignSession(s.store.Secret(), "admin", user.Username, adminTTL),
		Path:  "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(adminTTL / time.Second),
	})
	s.log.Info("admin password changed", "user", user.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// stats
// ---------------------------------------------------------------------------

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	settings := s.store.Settings()
	probe := s.store.Probe()
	stats := s.pipe.Stats()
	storeStats := s.store.Stats()

	tasks, _ := s.store.ListTasks(store.TaskFilter{Limit: 8})
	recent := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		recent = append(recent, taskSummary(t))
	}

	var probeOut any
	if probe != nil {
		probeOut = probe
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":              storeStats,
		"pipeline":           stats,
		"settings":           settings,
		"probe":              probeOut,
		"recent":             recent,
		"data_dir":           s.env.DataDir,
		"version":            Version,
		"time":               time.Now().UTC().Format(time.RFC3339),
		"effective_xff_mode": identity.EffectiveMode(settings, probe),
		"ipv6_supported":     probe != nil && probe.OK && probe.IPv6Accepted,
		"require_api_key":    settings.RequireAPIKey,
		"open_access":        !settings.RequireAPIKey && !s.store.HasEnabledKeys(),
	})
}

// Version is stamped by the build; overridable with -ldflags.
var Version = "dev"

func taskSummary(t *model.Task) map[string]any {
	return map[string]any{
		"id":         t.ID,
		"status":     t.Status,
		"model":      t.Model,
		"ratio":      t.Ratio,
		"duration":   t.Duration,
		"prompt":     t.Prompt,
		"forged_ip":  t.ForgedIP,
		"ip_family":  t.IPFamily,
		"attempts":   t.Attempts,
		"error":      t.Error,
		"created_at": t.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": t.UpdatedAt.UTC().Format(time.RFC3339),
		"api_key":    t.APIKeyTag,
		"source":     t.Source,
		"upstream":   t.UpstreamTaskID,
	}
}

// ---------------------------------------------------------------------------
// tasks
// ---------------------------------------------------------------------------

func (s *Server) handleAdminTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	tasks, total := s.store.ListTasks(store.TaskFilter{
		Status: q.Get("status"),
		Search: q.Get("q"),
		Limit:  limit,
		Offset: offset,
	})
	items := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		items = append(items, taskSummary(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  items,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

func (s *Server) handleAdminTaskGet(w http.ResponseWriter, r *http.Request) {
	t, err := s.lookupTask(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务不存在"})
		return
	}
	obj := taskSummary(t)
	obj["access_token"] = t.AccessToken
	obj["client_id"] = t.ClientID
	obj["visitor_id"] = t.VisitorID
	obj["video_cached"] = fileExists(s.pipe.VideoPath(t.ID))
	obj["input_cached"] = fileExists(s.pipe.UploadPath(t.ID))
	writeJSON(w, http.StatusOK, obj)
}

func (s *Server) handleAdminTaskDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.pipe.CancelTask(id)
	t, ok := s.store.DeleteTask(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务不存在"})
		return
	}
	_ = os.Remove(s.pipe.VideoPath(t.ID))
	_ = os.Remove(s.pipe.UploadPath(t.ID))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminTaskRetry(w http.ResponseWriter, r *http.Request) {
	task, err := s.pipe.Retry(r.Context(), r.PathValue("id"))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, taskSummary(task))
}

// ---------------------------------------------------------------------------
// api keys
// ---------------------------------------------------------------------------

func (s *Server) handleAdminKeys(w http.ResponseWriter, r *http.Request) {
	keys := s.store.ListKeys()
	items := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		items = append(items, keyView(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":           items,
		"require_api_key": s.store.Settings().RequireAPIKey,
		"open_access":     !s.store.Settings().RequireAPIKey && !s.store.HasEnabledKeys(),
	})
}

func keyView(k *model.APIKey) map[string]any {
	v := map[string]any{
		"id":                 k.ID,
		"name":               k.Name,
		"prefix":             auth.KeyPrefix(k.Key),
		"enabled":            k.Enabled,
		"note":               k.Note,
		"created_at":         k.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":         k.UpdatedAt.UTC().Format(time.RFC3339),
		"requests":           k.Requests,
		"rate_limit_per_min": k.RateLimitPerMin,
	}
	if !k.LastUsedAt.IsZero() {
		v["last_used_at"] = k.LastUsedAt.UTC().Format(time.RFC3339)
	}
	if k.ExpiresAt != nil {
		v["expires_at"] = k.ExpiresAt.UTC().Format(time.RFC3339)
		v["expired"] = k.Expired(time.Now())
	}
	return v
}

type keyCreateRequest struct {
	Name            string `json:"name"`
	Note            string `json:"note"`
	ExpiresInDays   int    `json:"expires_in_days"`
	RateLimitPerMin int    `json:"rate_limit_per_min"`
	Enabled         *bool  `json:"enabled"`
}

func (s *Server) handleAdminKeyCreate(w http.ResponseWriter, r *http.Request) {
	var req keyCreateRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "未命名密钥"
	}
	value, err := auth.GenerateAPIKey()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "生成密钥失败"})
		return
	}
	now := time.Now()
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	k := &model.APIKey{
		ID:              auth.GenerateID(),
		Name:            name,
		Key:             value,
		Enabled:         enabled,
		Note:            strings.TrimSpace(req.Note),
		CreatedAt:       now,
		UpdatedAt:       now,
		RateLimitPerMin: maxInt(req.RateLimitPerMin, 0),
	}
	if req.ExpiresInDays > 0 {
		exp := now.AddDate(0, 0, req.ExpiresInDays)
		k.ExpiresAt = &exp
	}
	s.store.AddKey(k)
	s.log.Info("api key created", "name", k.Name, "id", k.ID)

	view := keyView(k)
	// The full value is returned exactly once, at creation time.
	view["key"] = k.Key
	writeJSON(w, http.StatusCreated, view)
}

// handleAdminKeyReveal returns the full secret of one key. The list endpoint
// stays masked, so plaintext leaves the server only on this explicit request.
func (s *Server) handleAdminKeyReveal(w http.ResponseWriter, r *http.Request) {
	k, ok := s.store.GetKey(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "密钥不存在"})
		return
	}
	s.log.Info("api key revealed", "name", k.Name, "id", k.ID, "client", s.clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     k.ID,
		"name":   k.Name,
		"key":    k.Key,
		"prefix": auth.KeyPrefix(k.Key),
	})
}

type keyUpdateRequest struct {
	Name            *string `json:"name"`
	Note            *string `json:"note"`
	Enabled         *bool   `json:"enabled"`
	RateLimitPerMin *int    `json:"rate_limit_per_min"`
	ClearExpiry     bool    `json:"clear_expiry"`
	ExpiresInDays   *int    `json:"expires_in_days"`
}

func (s *Server) handleAdminKeyUpdate(w http.ResponseWriter, r *http.Request) {
	var req keyUpdateRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	updated, ok := s.store.UpdateKey(r.PathValue("id"), func(k *model.APIKey) {
		if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
			k.Name = strings.TrimSpace(*req.Name)
		}
		if req.Note != nil {
			k.Note = strings.TrimSpace(*req.Note)
		}
		if req.Enabled != nil {
			k.Enabled = *req.Enabled
		}
		if req.RateLimitPerMin != nil {
			k.RateLimitPerMin = maxInt(*req.RateLimitPerMin, 0)
		}
		if req.ClearExpiry {
			k.ExpiresAt = nil
		}
		if req.ExpiresInDays != nil && *req.ExpiresInDays > 0 {
			exp := time.Now().AddDate(0, 0, *req.ExpiresInDays)
			k.ExpiresAt = &exp
		}
	})
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "密钥不存在"})
		return
	}
	writeJSON(w, http.StatusOK, keyView(updated))
}

func (s *Server) handleAdminKeyDelete(w http.ResponseWriter, r *http.Request) {
	if !s.store.DeleteKey(r.PathValue("id")) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "密钥不存在"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// settings
// ---------------------------------------------------------------------------

func (s *Server) handleAdminSettingsGet(w http.ResponseWriter, r *http.Request) {
	settings := s.store.Settings()
	probe := s.store.Probe()
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":           settings,
		"effective_xff_mode": identity.EffectiveMode(settings, probe),
		"ipv6_supported":     probe != nil && probe.OK && probe.IPv6Accepted,
		"env": map[string]any{
			"host":     s.env.Host,
			"port":     s.env.Port,
			"data_dir": s.env.DataDir,
			"db_path":  s.env.DBPath,
		},
	})
}

func (s *Server) handleAdminSettingsPut(w http.ResponseWriter, r *http.Request) {
	var next config.Settings
	if err := readJSON(r, &next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误: " + err.Error()})
		return
	}
	if err := next.ValidateInput(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	next.Normalize()
	if err := next.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.store.UpdateSettings(next)
	s.log.Info("settings updated", "xff_mode", next.XFFMode, "endpoint_mode", next.EndpointMode,
		"max_concurrent", next.MaxConcurrent, "require_api_key", next.RequireAPIKey)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": s.store.Settings()})
}

// ---------------------------------------------------------------------------
// capability probe
// ---------------------------------------------------------------------------

func (s *Server) handleAdminProbeGet(w http.ResponseWriter, r *http.Request) {
	probe := s.store.Probe()
	writeJSON(w, http.StatusOK, map[string]any{"probe": probe})
}

type probeRequest struct {
	DryRun bool `json:"dry_run"`
}

func (s *Server) handleAdminProbeRun(w http.ResponseWriter, r *http.Request) {
	var req probeRequest
	_ = readJSON(r, &req)

	if !s.tryLockProbe() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "已有测试正在进行"})
		return
	}
	defer s.unlockProbe()

	opts := xffprobe.Options{DryRun: req.DryRun, Filename: "probe.jpg"}
	if !req.DryRun {
		img, err := s.probeImage()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "找不到可用的测试图片：" + err.Error(),
			})
			return
		}
		opts.Image = img
	}

	ctx, cancel := contextWithTimeout(r, 4*time.Minute)
	defer cancel()

	rec := s.runProbe(ctx, opts)
	s.log.Info("xff capability probe finished", "ipv4", rec.IPv4Accepted, "ipv6", rec.IPv6Accepted,
		"ok", rec.OK)
	writeJSON(w, http.StatusOK, map[string]any{"probe": rec})
}

// probeImage picks a bundled studio example to use as the probe payload.
func (s *Server) probeImage() ([]byte, error) {
	if s.webFS == nil {
		return nil, errors.New("未内置示例图片")
	}
	entries, err := fs.ReadDir(s.webFS, "web/studio/examples")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil, errors.New("示例目录为空")
	}
	sort.Strings(names)
	return fs.ReadFile(s.webFS, "web/studio/examples/"+names[0])
}

// ---------------------------------------------------------------------------
// upstream + rotator
// ---------------------------------------------------------------------------

func (s *Server) handleAdminUpstream(w http.ResponseWriter, r *http.Request) {
	settings := s.store.Settings()
	ctx, cancel := contextWithTimeout(r, 25*time.Second)
	defer cancel()

	out := map[string]any{
		"upstream":      settings.UpstreamBase,
		"usage_url":     settings.UsageURL(),
		"generate_url":  settings.GenerateURL(),
		"endpoint_mode": settings.EndpointMode,
	}
	start := time.Now()
	usage, err := s.up.Quota(ctx, "", "")
	out["latency_ms"] = time.Since(start).Milliseconds()
	if err != nil {
		out["ok"] = false
		out["error"] = err.Error()
	} else {
		out["ok"] = true
		out["usage"] = usage
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAdminRotatorReset(w http.ResponseWriter, r *http.Request) {
	s.rot.Reset()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = fmt.Sprintf
