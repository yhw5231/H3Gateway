package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yhw5231/H3Gateway/internal/auth"
	"github.com/yhw5231/H3Gateway/internal/config"
	"github.com/yhw5231/H3Gateway/internal/identity"
	"github.com/yhw5231/H3Gateway/internal/model"
	"github.com/yhw5231/H3Gateway/internal/pipeline"
	"github.com/yhw5231/H3Gateway/internal/store"
	"github.com/yhw5231/H3Gateway/internal/upstream"
)

var jpegBytes = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}

const adminPassword = "admin"

// fakeUpstream answers the trial endpoints so the whole HTTP surface can be
// exercised without touching the network.
func fakeUpstreamHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/minimax-trial/usage", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"enabled": true, "limit": 2, "used": 0, "remaining": 2, "max_concurrent": 5})
	})
	mux.HandleFunc("/api/minimax-trial/video-generation", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(8 << 20)
		writeTestJSON(w, map[string]any{
			"task_id": "up-1", "access_token": "tok", "status": "queued",
			"limit": 2, "remaining": 1, "max_concurrent": 5,
		})
	})
	mux.HandleFunc("/api/minimax-trial/video-generation/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/content") {
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("MP4DATA"))
			return
		}
		writeTestJSON(w, map[string]any{"status": "succeeded", "task_id": "up-1"})
	})
	return mux
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type testEnv struct {
	srv    *Server
	store  *store.Store
	pipe   *pipeline.Pipeline
	ts     *httptest.Server
	client *http.Client
}

func newTestEnv(t *testing.T, mutate func(*config.Env, *config.Settings)) *testEnv {
	t.Helper()

	up := httptest.NewServer(fakeUpstreamHandler())
	t.Cleanup(up.Close)

	dir := t.TempDir()
	settings := config.DefaultSettings()
	settings.UpstreamBase = up.URL
	settings.PollIntervalSec = 0.5
	settings.SubmitTimeoutSec = 15
	if mutate != nil {
		var e config.Env
		mutate(&e, &settings)
	}
	settings.Normalize()

	env := config.LoadEnv()
	env.DataDir = dir
	env.DBPath = filepath.Join(dir, "h3gateway.json")
	env.VideosDir = filepath.Join(dir, "videos")
	env.UploadsDir = filepath.Join(dir, "uploads")
	env.AdminUser = "admin"
	env.AdminPassword = adminPassword
	env.SessionSecret = "test-secret"
	if mutate != nil {
		mutate(&env, &settings)
	}

	st, err := store.Open(env.DBPath, settings)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.OverrideSecret(env.SessionSecret)

	// Seed the console account exactly like main.bootstrapAccounts does.
	hash, salt, err := auth.HashPassword(env.AdminPassword, 1000)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	st.UpsertUser(&model.AdminUser{
		Username: env.AdminUser, PasswordHash: hash, Salt: salt, Iterations: 1000,
		MustChangePassword: env.AdminPassword == "admin",
		UpdatedAt:          time.Now(),
	})

	upc := upstream.New(st.Settings)
	t.Cleanup(upc.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipe := pipeline.New(st, upc, identity.NewRotator(), log, env.VideosDir, env.UploadsDir, st.Probe)
	ctx, cancel := context.WithCancel(context.Background())
	pipe.Start(ctx)
	t.Cleanup(func() { cancel(); pipe.Stop() })

	web := fstest.MapFS{
		"web/studio/index.html":     &fstest.MapFile{Data: []byte("<html>studio</html>")},
		"web/studio/app.js":         &fstest.MapFile{Data: []byte("console.log(1)")},
		"web/studio/styles.css":     &fstest.MapFile{Data: []byte("body{}")},
		"web/studio/examples/a.jpg": &fstest.MapFile{Data: jpegBytes},
		"web/admin/index.html":      &fstest.MapFile{Data: []byte("<html>admin</html>")},
		"web/admin/admin.js":        &fstest.MapFile{Data: []byte("//admin")},
		"web/admin/admin.css":       &fstest.MapFile{Data: []byte("body{}")},
	}

	srv := New(env, st, pipe, upc, pipe.Rotator(), log, web)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &testEnv{srv: srv, store: st, pipe: pipe, ts: ts, client: ts.Client()}
}

// do performs a request. body may be nil, a string, or a value to be JSON
// encoded. mutate can add headers.
func (e *testEnv) do(t *testing.T, method, path string, body any, mutate func(*http.Request)) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	contentType := ""
	switch v := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(v)
		contentType = "application/json"
	case []byte:
		reader = strings.NewReader(string(v))
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = strings.NewReader(string(raw))
		contentType = "application/json"
	}

	req, err := http.NewRequest(method, e.ts.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if mutate != nil {
		mutate(req)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func decode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode %q: %v", string(data), err)
	}
	return out
}

// login authenticates the console and returns the session cookie.
func (e *testEnv) login(t *testing.T) *http.Cookie {
	t.Helper()
	resp, data := e.do(t, "POST", "/admin/api/login",
		map[string]string{"username": "admin", "password": adminPassword},
		func(r *http.Request) { r.Header.Set("X-Requested-With", "h3admin") })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d %s", resp.StatusCode, data)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "h3_admin" {
			return c
		}
	}
	t.Fatal("login did not set the h3_admin cookie")
	return nil
}

func withCookie(c *http.Cookie) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(c) }
}

func adminHeaders(c *http.Cookie) func(*http.Request) {
	return func(r *http.Request) {
		r.AddCookie(c)
		r.Header.Set("X-Requested-With", "h3admin")
	}
}

// ---------------------------------------------------------------------------
// health and public surface
// ---------------------------------------------------------------------------

func TestHealthEndpoint(t *testing.T) {
	e := newTestEnv(t, nil)
	for _, path := range []string{"/health", "/healthz"} {
		resp, data := e.do(t, "GET", path, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d", path, resp.StatusCode)
		}
		body := decode(t, data)
		if body["status"] != "ok" {
			t.Fatalf("%s body = %v", path, body)
		}
	}
}

func TestStudioPageSetsSessionCookie(t *testing.T) {
	e := newTestEnv(t, nil)
	resp, data := e.do(t, "GET", "/", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "studio") {
		t.Fatalf("unexpected studio page: %s", data)
	}
	var found bool
	for _, c := range resp.Cookies() {
		if c.Name == "h3_studio" {
			found = true
			if !c.HttpOnly {
				t.Fatal("studio cookie must be HttpOnly")
			}
		}
	}
	if !found {
		t.Fatal("studio page did not mint the h3_studio cookie")
	}
}

func TestStudioAssetsAreServed(t *testing.T) {
	e := newTestEnv(t, nil)
	for _, path := range []string{"/studio/app.js", "/studio/styles.css", "/studio/examples/a.jpg"} {
		resp, _ := e.do(t, "GET", path, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d", path, resp.StatusCode)
		}
	}
}

func TestStudioAssetTraversalIsBlocked(t *testing.T) {
	e := newTestEnv(t, nil)
	resp, _ := e.do(t, "GET", "/studio/../../../etc/passwd", nil, nil)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("path traversal was served")
	}
}

func TestAdminAssetsAreServedWithTheRightContentType(t *testing.T) {
	// The console is useless if these come back as the HTML shell: a browser
	// would refuse to execute the script and the stylesheet would not apply.
	// Asserting only the status code is not enough, so check the body and type.
	e := newTestEnv(t, nil)
	cases := []struct{ path, wantType, wantBody string }{
		{"/admin/admin.js", "application/javascript", "//admin"},
		{"/admin/admin.css", "text/css", "body{}"},
	}
	for _, c := range cases {
		resp, data := e.do(t, "GET", c.path, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d", c.path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, c.wantType) {
			t.Errorf("%s Content-Type = %q, want %q", c.path, ct, c.wantType)
		}
		if got := string(data); got != c.wantBody {
			t.Errorf("%s body = %q, want %q", c.path, got, c.wantBody)
		}
	}
}

func TestMissingAdminAssetIs404NotTheShell(t *testing.T) {
	e := newTestEnv(t, nil)
	for _, path := range []string{"/admin/nope.js", "/admin/deep/nested.js", "/admin/"} {
		if path == "/admin/" {
			continue // the console itself is legitimately served here
		}
		resp, data := e.do(t, "GET", path, nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, resp.StatusCode)
		}
		if strings.Contains(string(data), "<!doctype html") {
			t.Errorf("%s served the HTML shell", path)
		}
	}
	// An unknown path under the subtree must not fall back to the shell either.
	resp, data := e.do(t, "GET", "/admin/deep/nested.js", nil, nil)
	if resp.StatusCode != http.StatusNotFound || strings.Contains(string(data), "<!doctype html") {
		t.Fatalf("nested unknown path = %d %q", resp.StatusCode, data)
	}
}

// consoleFiles reads the real console sources. The unit-test web FS is a stub,
// so contract checks have to look at the shipped files.
func consoleFiles(t *testing.T) (string, string) {
	t.Helper()
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join("..", "..", "web", "admin", name))
		if err != nil {
			t.Fatalf("read web/admin/%s: %v", name, err)
		}
		return string(data)
	}
	return read("index.html"), read("admin.js")
}

// TestConsoleSelectorsResolve guards the DOM contract between the console markup
// and its script: a renamed element id otherwise fails silently in the browser.
func TestConsoleSelectorsResolve(t *testing.T) {
	html, js := consoleFiles(t)

	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`id="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		declared[m[1]] = true
	}
	// Ids the script builds itself at runtime are legitimately absent from the
	// static markup.
	created := map[string]bool{}
	for _, m := range regexp.MustCompile(`id="([^"]+)"`).FindAllStringSubmatch(js, -1) {
		created[m[1]] = true
	}

	var checked int
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`\$\("#([A-Za-z0-9_-]+)"\)`),
		regexp.MustCompile(`getElementById\("([A-Za-z0-9_-]+)"\)`),
	} {
		for _, m := range re.FindAllStringSubmatch(js, -1) {
			checked++
			if !declared[m[1]] && !created[m[1]] {
				t.Errorf("admin.js selects #%s but index.html never declares it", m[1])
			}
		}
	}
	if checked < 30 {
		t.Fatalf("only %d selectors found; the parser probably missed them", checked)
	}
}

// TestConsoleEndpointsExist ensures every admin endpoint the console calls is
// actually routed. Without it, a renamed route shows up only as a 404 toast.
func TestConsoleEndpointsExist(t *testing.T) {
	e := newTestEnv(t, nil)
	_, js := consoleFiles(t)

	type call struct{ method, path string }
	calls := map[call]bool{}
	withOpts := regexp.MustCompile("api\\(\\s*[`\"]([^`\"]+)[`\"]\\s*,\\s*\\{([^}]*)\\}")
	for _, m := range withOpts.FindAllStringSubmatch(js, -1) {
		method := "GET"
		if mm := regexp.MustCompile(`method:\s*"([A-Z]+)"`).FindStringSubmatch(m[2]); mm != nil {
			method = mm[1]
		}
		calls[call{method, m[1]}] = true
	}
	plain := regexp.MustCompile("api\\(\\s*[`\"](/admin/api/[^`\"]*)[`\"]\\s*\\)")
	for _, m := range plain.FindAllStringSubmatch(js, -1) {
		calls[call{"GET", m[1]}] = true
	}
	if len(calls) < 10 {
		t.Fatalf("only %d console calls parsed; expected the full surface", len(calls))
	}

	for c := range calls {
		// Substitute a placeholder for template segments.
		path := regexp.MustCompile(`\$\{[^}]*\}`).ReplaceAllString(c.path, "x")
		path = strings.SplitN(path, "?", 2)[0]
		// No cookie: a routed endpoint answers 401/403, never 404/405.
		resp, _ := e.do(t, c.method, path, nil, nil)
		switch resp.StatusCode {
		case http.StatusNotFound, http.StatusMethodNotAllowed:
			t.Errorf("console calls %s %s but the server answered %d", c.method, c.path, resp.StatusCode)
		}
	}
}

func TestAdminPageIsServed(t *testing.T) {
	e := newTestEnv(t, nil)
	resp, data := e.do(t, "GET", "/admin", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin = %d", resp.StatusCode)
	}
	if !strings.Contains(string(data), "admin") {
		t.Fatalf("unexpected admin page: %s", data)
	}
}

// ---------------------------------------------------------------------------
// API authentication
// ---------------------------------------------------------------------------

func TestOpenAccessWhenNoKeysAndNotRequired(t *testing.T) {
	e := newTestEnv(t, nil)
	resp, _ := e.do(t, "GET", "/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected open access, got %d", resp.StatusCode)
	}
}

func TestKeyRequiredOnceAKeyExists(t *testing.T) {
	e := newTestEnv(t, nil)
	key, _ := auth.GenerateAPIKey()
	now := time.Now()
	e.store.AddKey(&model.APIKey{ID: "k1", Name: "prod", Key: key, Enabled: true, CreatedAt: now, UpdatedAt: now})

	resp, _ := e.do(t, "GET", "/v1/models", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing key should be rejected, got %d", resp.StatusCode)
	}

	resp, _ = e.do(t, "GET", "/v1/models", nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+key)
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid key rejected: %d", resp.StatusCode)
	}

	// A wrong key must never fall through to open access.
	resp, _ = e.do(t, "GET", "/v1/models", nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer h3-wrong")
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid key accepted: %d", resp.StatusCode)
	}
}

func TestRequireAPIKeyFlagBlocksAnonymous(t *testing.T) {
	e := newTestEnv(t, func(_ *config.Env, s *config.Settings) { s.RequireAPIKey = true })
	resp, _ := e.do(t, "GET", "/v1/models", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestDisabledKeyIsRejected(t *testing.T) {
	e := newTestEnv(t, nil)
	key, _ := auth.GenerateAPIKey()
	now := time.Now()
	e.store.AddKey(&model.APIKey{ID: "k1", Name: "off", Key: key, Enabled: false, CreatedAt: now, UpdatedAt: now})

	resp, _ := e.do(t, "GET", "/v1/models", nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+key)
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled key accepted: %d", resp.StatusCode)
	}
}

func TestStudioCookieGrantsAPIccess(t *testing.T) {
	e := newTestEnv(t, nil)
	_, _ = e.do(t, "GET", "/", nil, nil)
	// Re-fetch to capture the cookie value.
	resp, _ := e.do(t, "GET", "/", nil, nil)
	var studio *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "h3_studio" {
			studio = c
		}
	}
	if studio == nil {
		t.Fatal("no studio cookie")
	}
	got, _ := e.do(t, "GET", "/v1/models", nil, withCookie(studio))
	if got.StatusCode != http.StatusOK {
		t.Fatalf("studio session rejected: %d", got.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// video API
// ---------------------------------------------------------------------------

func TestCreateVideoFromDataURL(t *testing.T) {
	e := newTestEnv(t, nil)
	dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegBytes)
	resp, body := e.do(t, "POST", "/v1/videos", map[string]any{
		"model": "minimax-h3", "image": dataURL, "prompt": "animate it", "duration": 6,
	}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	out := decode(t, body)
	id, _ := out["id"].(string)
	if !strings.HasPrefix(id, "video_") {
		t.Fatalf("id = %q", id)
	}
	if out["status"] != model.StatusQueued {
		t.Fatalf("status = %v", out["status"])
	}

	// Poll the task until it settles.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, raw := e.do(t, "GET", "/v1/videos/"+id, nil, nil)
		cur := decode(t, raw)
		if cur["status"] == model.StatusSucceeded {
			break
		}
		if cur["status"] == model.StatusFailed {
			t.Fatalf("task failed: %v", cur)
		}
		time.Sleep(50 * time.Millisecond)
	}

	resp, raw := e.do(t, "GET", "/v1/videos/"+id+"/content", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("content = %d %s", resp.StatusCode, raw)
	}
	if string(raw) != "MP4DATA" {
		t.Fatalf("content = %q", raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "video/mp4") {
		t.Fatalf("content type = %q", ct)
	}
}

func TestCreateVideoValidation(t *testing.T) {
	e := newTestEnv(t, nil)

	resp, body := e.do(t, "POST", "/v1/videos", map[string]any{"model": "minimax-h3"}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing image = %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "image") {
		t.Fatalf("error should mention image: %s", body)
	}

	resp, _ = e.do(t, "POST", "/v1/videos", map[string]any{
		"image": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte("not an image")),
	}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-image payload = %d", resp.StatusCode)
	}

	resp, _ = e.do(t, "POST", "/v1/videos", "not json at all", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body = %d", resp.StatusCode)
	}
}

func TestGetUnknownVideoReturns404(t *testing.T) {
	e := newTestEnv(t, nil)
	resp, _ := e.do(t, "GET", "/v1/videos/video_nope", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown video = %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "GET", "/v1/videos/video_nope/content", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown content = %d", resp.StatusCode)
	}
}

func TestTrialUsageEndpoint(t *testing.T) {
	e := newTestEnv(t, nil)
	resp, body := e.do(t, "GET", "/v1/trial/usage", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("usage = %d %s", resp.StatusCode, body)
	}
	out := decode(t, body)
	if out["max_concurrent"] != float64(4) {
		t.Fatalf("usage body = %v", out)
	}
	if out["effective_xff_mode"] != config.XFFIPv4 {
		t.Fatalf("effective mode = %v", out["effective_xff_mode"])
	}
	if out["ipv6_supported"] != false {
		t.Fatalf("ipv6_supported = %v", out["ipv6_supported"])
	}
	// Operational counters must not be exposed on the public surface.
	for _, leaky := range []string{"identities_minted", "identities_exhausted", "tasks"} {
		if _, ok := out[leaky]; ok {
			t.Fatalf("public usage endpoint leaked %q: %v", leaky, out)
		}
	}
}

func TestModelsEndpointListsOpenAIShape(t *testing.T) {
	e := newTestEnv(t, nil)
	resp, body := e.do(t, "GET", "/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models = %d", resp.StatusCode)
	}
	out := decode(t, body)
	list, ok := out["data"].([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("models body = %v", out)
	}
	first, _ := list[0].(map[string]any)
	if first["object"] != "model" || first["id"] == "" {
		t.Fatalf("model entry = %v", first)
	}
}

func TestChatCompletionsRejectsMissingPrompt(t *testing.T) {
	e := newTestEnv(t, nil)
	resp, body := e.do(t, "POST", "/v1/chat/completions", map[string]any{
		"model": "minimax-h3", "messages": []any{},
	}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty messages = %d %s", resp.StatusCode, body)
	}
}

func TestChatCompletionsCreatesTask(t *testing.T) {
	e := newTestEnv(t, nil)
	dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegBytes)
	resp, body := e.do(t, "POST", "/v1/chat/completions", map[string]any{
		"model": "minimax-h3",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "animate this"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
			}},
		},
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat = %d %s", resp.StatusCode, body)
	}
	out := decode(t, body)
	choices, _ := out["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("chat body = %v", out)
	}
	first, _ := choices[0].(map[string]any)
	msg, _ := first["message"].(map[string]any)
	if msg == nil || msg["content"] == "" {
		t.Fatalf("chat message = %v", first)
	}
}

// ---------------------------------------------------------------------------
// admin console
// ---------------------------------------------------------------------------

func TestAdminLoginRejectsBadCredentials(t *testing.T) {
	e := newTestEnv(t, nil)
	resp, _ := e.do(t, "POST", "/admin/api/login",
		map[string]string{"username": "admin", "password": "wrong"},
		func(r *http.Request) { r.Header.Set("X-Requested-With", "h3admin") })
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad password = %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "POST", "/admin/api/login",
		map[string]string{"username": "nobody", "password": "admin"},
		func(r *http.Request) { r.Header.Set("X-Requested-With", "h3admin") })
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown user = %d", resp.StatusCode)
	}
}

func TestAdminEndpointsRequireAuthentication(t *testing.T) {
	e := newTestEnv(t, nil)
	for _, path := range []string{"/admin/api/stats", "/admin/api/tasks", "/admin/api/keys", "/admin/api/settings", "/admin/api/probe"} {
		resp, _ := e.do(t, "GET", path, nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a session = %d", path, resp.StatusCode)
		}
	}
}

func TestAdminMutationsRequireSameOriginMarker(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.login(t)

	// A cross-site form post would not carry the custom header.
	resp, _ := e.do(t, "POST", "/admin/api/keys",
		map[string]string{"name": "sneaky"}, withCookie(cookie))
	if resp.StatusCode == http.StatusOK {
		t.Fatal("state-changing request without X-Requested-With was accepted")
	}
}

func TestAdminSessionAndLogout(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.login(t)

	resp, body := e.do(t, "GET", "/admin/api/session", nil, withCookie(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session = %d", resp.StatusCode)
	}
	out := decode(t, body)
	if out["username"] != "admin" {
		t.Fatalf("session body = %v", out)
	}
	// The seeded default password must be flagged.
	if out["must_change_password"] != true {
		t.Fatalf("must_change_password = %v", out["must_change_password"])
	}

	resp, _ = e.do(t, "POST", "/admin/api/logout", nil, adminHeaders(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
	// Sessions are stateless HMAC tokens, so logout clears the cookie; the
	// browser must then be refused.
	var cleared bool
	for _, c := range resp.Cookies() {
		if c.Name == "h3_admin" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not clear the h3_admin cookie")
	}
	resp, _ = e.do(t, "GET", "/admin/api/session", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("console reachable without a cookie: %d", resp.StatusCode)
	}
}

func TestDefaultPasswordLocksDownTheConsole(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.login(t)

	// While the default password is in place only the password change is allowed.
	resp, body := e.do(t, "GET", "/admin/api/keys", nil, withCookie(cookie))
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("console should be locked down until the password changes: %s", body)
	}
}

func TestPasswordChangeUnlocksAndReissuesSession(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.login(t)

	resp, body := e.do(t, "POST", "/admin/api/password",
		map[string]string{"current_password": adminPassword, "new_password": "a-much-better-password"},
		adminHeaders(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("password change = %d %s", resp.StatusCode, body)
	}

	var fresh *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "h3_admin" {
			fresh = c
		}
	}
	if fresh == nil {
		t.Fatal("password change did not reissue the session cookie")
	}

	resp, body = e.do(t, "GET", "/admin/api/keys", nil, withCookie(fresh))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("console still locked after the change: %d %s", resp.StatusCode, body)
	}

	// The old password must no longer work.
	resp, _ = e.do(t, "POST", "/admin/api/login",
		map[string]string{"username": "admin", "password": adminPassword},
		func(r *http.Request) { r.Header.Set("X-Requested-With", "h3admin") })
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password still accepted: %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "POST", "/admin/api/login",
		map[string]string{"username": "admin", "password": "a-much-better-password"},
		func(r *http.Request) { r.Header.Set("X-Requested-With", "h3admin") })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new password rejected: %d", resp.StatusCode)
	}
}

func TestPasswordChangeValidatesInput(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.login(t)
	resp, body := e.do(t, "POST", "/admin/api/password",
		map[string]string{"current_password": adminPassword, "new_password": "short"},
		adminHeaders(cookie))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("short password = %d %s", resp.StatusCode, body)
	}
	resp, _ = e.do(t, "POST", "/admin/api/password",
		map[string]string{"current_password": "wrong", "new_password": "a-much-better-password"},
		adminHeaders(cookie))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong current password = %d", resp.StatusCode)
	}
}

// unlock returns a console session for a store whose password has been changed.
func (e *testEnv) unlock(t *testing.T) *http.Cookie {
	t.Helper()
	cookie := e.login(t)
	resp, body := e.do(t, "POST", "/admin/api/password",
		map[string]string{"current_password": adminPassword, "new_password": "a-much-better-password"},
		adminHeaders(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unlock failed: %d %s", resp.StatusCode, body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "h3_admin" {
			return c
		}
	}
	t.Fatal("no session cookie after unlock")
	return nil
}

func TestSessionIsRevokedWhenTheAccountDisappears(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.unlock(t)

	resp, _ := e.do(t, "GET", "/admin/api/keys", nil, withCookie(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session should work before deletion: %d", resp.StatusCode)
	}
	// Drop the account; the still-signed cookie must stop working at once.
	if !e.store.DeleteUser("admin") {
		t.Fatal("DeleteUser missed")
	}
	resp, _ = e.do(t, "GET", "/admin/api/keys", nil, withCookie(cookie))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session survived account deletion: %d", resp.StatusCode)
	}
}

func TestAdminStatsShape(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.unlock(t)
	resp, body := e.do(t, "GET", "/admin/api/stats", nil, withCookie(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats = %d %s", resp.StatusCode, body)
	}
	out := decode(t, body)
	for _, key := range []string{"tasks", "pipeline", "settings", "effective_xff_mode", "ipv6_supported"} {
		if _, ok := out[key]; !ok {
			t.Fatalf("stats is missing %q: %v", key, out)
		}
	}
	if out["ipv6_supported"] != false {
		t.Fatalf("ipv6_supported should be false before any probe: %v", out["ipv6_supported"])
	}
}

func TestAdminKeyLifecycle(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.unlock(t)

	resp, body := e.do(t, "POST", "/admin/api/keys",
		map[string]any{"name": "production", "note": "ci", "rate_limit_per_min": 30},
		adminHeaders(cookie))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("create key = %d %s", resp.StatusCode, body)
	}
	created := decode(t, body)
	secret, _ := created["key"].(string)
	if !strings.HasPrefix(secret, "h3-") {
		t.Fatalf("created key = %v", created)
	}
	id, _ := created["id"].(string)

	// The secret must work immediately.
	resp, _ = e.do(t, "GET", "/v1/models", nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+secret)
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new key rejected: %d", resp.StatusCode)
	}

	// Listing must not leak the secret again.
	resp, body = e.do(t, "GET", "/admin/api/keys", nil, withCookie(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list keys = %d", resp.StatusCode)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("key list leaked the secret: %s", body)
	}
	if !strings.Contains(string(body), "production") {
		t.Fatalf("key list missing the new key: %s", body)
	}

	// The explicit reveal endpoint hands the plaintext back for display/copy.
	resp, _ = e.do(t, "GET", "/admin/api/keys/"+id+"/secret", nil, nil)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("reveal without a session = %d, want an auth failure", resp.StatusCode)
	}
	resp, body = e.do(t, "GET", "/admin/api/keys/"+id+"/secret", nil, withCookie(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reveal key = %d %s", resp.StatusCode, body)
	}
	revealed := decode(t, body)
	if got, _ := revealed["key"].(string); got != secret {
		t.Fatalf("revealed key = %q, want the created secret", got)
	}
	if got, _ := revealed["id"].(string); got != id {
		t.Fatalf("revealed id = %q, want %q", got, id)
	}
	resp, _ = e.do(t, "GET", "/admin/api/keys/does-not-exist/secret", nil, withCookie(cookie))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("reveal unknown key = %d, want 404", resp.StatusCode)
	}

	// Disable it.
	resp, body = e.do(t, "PATCH", "/admin/api/keys/"+id, map[string]any{"enabled": false}, adminHeaders(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch key = %d %s", resp.StatusCode, body)
	}
	resp, _ = e.do(t, "GET", "/v1/models", nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+secret)
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled key still works: %d", resp.StatusCode)
	}

	// Delete it.
	resp, _ = e.do(t, "DELETE", "/admin/api/keys/"+id, nil, adminHeaders(cookie))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete key = %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "DELETE", "/admin/api/keys/"+id, nil, adminHeaders(cookie))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete = %d", resp.StatusCode)
	}
}

func TestAdminKeyCreationAppliesADefaultName(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.unlock(t)
	resp, body := e.do(t, "POST", "/admin/api/keys", map[string]any{"name": "   "}, adminHeaders(cookie))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("blank name = %d %s", resp.StatusCode, body)
	}
	if name, _ := decode(t, body)["name"].(string); strings.TrimSpace(name) == "" {
		t.Fatalf("expected a fallback name, got %v", decode(t, body))
	}
}

func TestAdminSettingsRoundTrip(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.unlock(t)

	resp, body := e.do(t, "GET", "/admin/api/settings", nil, withCookie(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get settings = %d", resp.StatusCode)
	}
	settings, _ := decode(t, body)["settings"].(map[string]any)
	if settings == nil {
		t.Fatal("settings missing from the response")
	}
	settings["xff_mode"] = config.XFFIPv6
	settings["max_concurrent"] = float64(6)

	resp, body = e.do(t, "PUT", "/admin/api/settings", settings, adminHeaders(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put settings = %d %s", resp.StatusCode, body)
	}
	got := e.store.Settings()
	if got.XFFMode != config.XFFIPv6 || got.MaxConcurrent != 6 {
		t.Fatalf("settings not applied: %+v", got)
	}
}

func TestAdminSettingsRejectInvalidValues(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.unlock(t)
	for _, body := range []map[string]any{
		{"max_concurrent": 9999},
		{"max_concurrent": -1},
		{"submit_timeout_sec": 1},
		{"poll_interval_sec": 5000},
		{"endpoint_mode": "bogus"},
		{"xff_mode": "bogus"},
		{"xff_pool": "bogus"},
		{"task_resubmits": 5000},
	} {
		resp, raw := e.do(t, "PUT", "/admin/api/settings", body, adminHeaders(cookie))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("settings %v = %d %s", body, resp.StatusCode, raw)
		}
	}
	// A rejected write must not have changed anything.
	if got := e.store.Settings(); got.MaxConcurrent != 4 {
		t.Fatalf("rejected settings were applied: %+v", got)
	}
}

func TestAdminTaskListAndDelete(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.unlock(t)

	now := time.Now()
	e.store.SaveTask(&model.Task{ID: "video_one", Status: model.StatusSucceeded, Model: "minimax-h3",
		Ratio: "9:16", Duration: 6, ForgedIP: "203.0.113.5", IPFamily: model.FamilyIPv4,
		CreatedAt: now, UpdatedAt: now})

	resp, body := e.do(t, "GET", "/admin/api/tasks?limit=10", nil, withCookie(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list tasks = %d", resp.StatusCode)
	}
	out := decode(t, body)
	if out["total"] != float64(1) {
		t.Fatalf("task list = %v", out)
	}

	resp, body = e.do(t, "GET", "/admin/api/tasks/video_one", nil, withCookie(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get task = %d", resp.StatusCode)
	}
	task := decode(t, body)
	if task["forged_ip"] != "203.0.113.5" {
		t.Fatalf("task = %v", task)
	}

	resp, _ = e.do(t, "DELETE", "/admin/api/tasks/video_one", nil, adminHeaders(cookie))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete task = %d", resp.StatusCode)
	}
	if _, ok := e.store.GetTask("video_one"); ok {
		t.Fatal("task survived deletion")
	}
}

func TestAdminProbeStatusBeforeAnyRun(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.unlock(t)
	resp, body := e.do(t, "GET", "/admin/api/probe", nil, withCookie(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("probe = %d", resp.StatusCode)
	}
	out := decode(t, body)
	if out["probe"] != nil {
		t.Fatalf("expected no probe record, got %v", out["probe"])
	}
}

func TestAdminProbeDryRunStoresNoVerdict(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.unlock(t)
	resp, body := e.do(t, "POST", "/admin/api/probe", map[string]any{"dry_run": true}, adminHeaders(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dry run probe = %d %s", resp.StatusCode, body)
	}
	out := decode(t, body)
	probe, _ := out["probe"].(map[string]any)
	if probe == nil {
		t.Fatalf("dry run returned no result: %v", out)
	}
	if probe["ok"] != true {
		t.Fatalf("dry run should reach the fake upstream: %v", probe)
	}
	// A dry run must not claim either family is a quota key: nothing was
	// submitted, so the only honest answer is "unverified".
	if probe["ipv4_accepted"] == true || probe["ipv6_accepted"] == true {
		t.Fatalf("dry run must not assert quota-key support: %v", probe)
	}
	if probe["dry_run"] != true {
		t.Fatalf("dry run flag missing: %v", probe)
	}
	// It must also not be persisted as a capability verdict.
	if rec := e.store.Probe(); rec != nil && rec.IPv6Accepted {
		t.Fatalf("dry run persisted an IPv6 verdict: %+v", rec)
	}
}

func TestAdminUpstreamStatus(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.unlock(t)
	resp, body := e.do(t, "GET", "/admin/api/upstream", nil, withCookie(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upstream = %d", resp.StatusCode)
	}
	out := decode(t, body)
	if out["ok"] != true {
		t.Fatalf("upstream probe failed: %v", out)
	}
	if out["generate_url"] == "" || out["usage_url"] == "" {
		t.Fatalf("upstream body = %v", out)
	}
}

func TestAdminRotatorReset(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.unlock(t)
	_ = e.pipe.Rotator().Mint(model.FamilyIPv4, identity.PoolPublic, false)
	resp, body := e.do(t, "POST", "/admin/api/rotator/reset", nil, adminHeaders(cookie))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotator reset = %d %s", resp.StatusCode, body)
	}
	if st := e.pipe.Rotator().Stats(); st.IdentitiesMinted != 0 {
		t.Fatalf("rotator was not reset: %+v", st)
	}
}

func TestAdminRetryTask(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.unlock(t)

	now := time.Now()
	e.store.SaveTask(&model.Task{ID: "video_retry", Status: model.StatusFailed, Model: "minimax-h3",
		Ratio: "9:16", Duration: 6, CreatedAt: now, UpdatedAt: now})
	// Retry needs the cached input image.
	if err := os.MkdirAll(e.pipe.UploadPath(""), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(e.pipe.UploadPath("video_retry"), jpegBytes, 0o600); err != nil {
		t.Fatalf("seed upload: %v", err)
	}

	resp, body := e.do(t, "POST", "/admin/api/tasks/video_retry/retry", nil, adminHeaders(cookie))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("retry = %d %s", resp.StatusCode, body)
	}
	_, total := e.store.ListTasks(store.TaskFilter{})
	if total != 2 {
		t.Fatalf("retry should create a second task, total = %d", total)
	}
}

func TestAdminRetryUnknownTask(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.unlock(t)
	resp, _ := e.do(t, "POST", "/admin/api/tasks/video_nope/retry", nil, adminHeaders(cookie))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("retry unknown = %d", resp.StatusCode)
	}
}

func TestUnknownAdminEndpointReturns404(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.unlock(t)
	resp, _ := e.do(t, "GET", "/admin/api/does-not-exist", nil, withCookie(cookie))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown endpoint = %d", resp.StatusCode)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	e := newTestEnv(t, nil)
	resp, _ := e.do(t, "DELETE", "/health", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /health = %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// helpers used by the handlers
// ---------------------------------------------------------------------------

func TestNormalizeRatio(t *testing.T) {
	ok := []string{"9:16", "720x1280", "1080x1920", "vertical", "portrait", "  9:16  ", "VERTICAL", ""}
	for _, in := range ok {
		got, err := normalizeRatio(in)
		if err != nil {
			t.Fatalf("normalizeRatio(%q) errored: %v", in, err)
		}
		if got != "9:16" {
			t.Fatalf("normalizeRatio(%q) = %q, want 9:16", in, got)
		}
	}
	// The trial channel is portrait-only, so anything else must be refused
	// rather than silently coerced.
	for _, in := range []string{"16:9", "1:1", "landscape", "nonsense"} {
		if _, err := normalizeRatio(in); err == nil {
			t.Fatalf("normalizeRatio(%q) should have failed", in)
		}
	}
}

func TestNormalizeDuration(t *testing.T) {
	cases := []struct {
		value string
		model string
		want  int
	}{
		{"", "minimax-h3", 6},
		{"6", "minimax-h3", 6},
		{"6s", "minimax-h3", 6},
		{"4", "minimax-h3", 6},
		{"10", "minimax-h3", 10},
		{"10s", "minimax-h3", 10},
		{"12", "minimax-h3", 10},
		{"15", "minimax-h3", 15},
		{"20", "minimax-h3", 15},
		{"9.0", "minimax-h3", 10},
		// A model suffix wins over the body, matching the Python original.
		{"6", "minimax-h3-10s", 10},
		{"", "minimax-h3-15s", 15},
		{"6", "h3-15s", 15},
	}
	for _, tc := range cases {
		got, err := normalizeDuration(tc.value, tc.model)
		if err != nil {
			t.Fatalf("normalizeDuration(%q, %q) errored: %v", tc.value, tc.model, err)
		}
		if got != tc.want {
			t.Fatalf("normalizeDuration(%q, %q) = %d, want %d", tc.value, tc.model, got, tc.want)
		}
	}
	for _, bad := range []string{"abc", "3", "17", "-6"} {
		if _, err := normalizeDuration(bad, "minimax-h3"); err == nil {
			t.Fatalf("normalizeDuration(%q) should have failed", bad)
		}
	}
}

func TestRawToString(t *testing.T) {
	cases := map[string]string{
		`"hello"`:         "hello",
		`  " padded "  `:  "padded",
		`123`:             "123",
		`1.5`:             "1.5",
		``:                "",
		`null`:            "",
		`{"url":"a.jpg"}`: "a.jpg",
		`{"url":"  b "}`:  "b",
		`{"other":"x"}`:   "",
		`[1,2,3]`:         "",
	}
	for in, want := range cases {
		if got := rawToString([]byte(in)); got != want {
			t.Fatalf("rawToString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBaseURLPrefersConfiguredPublicURL(t *testing.T) {
	e := newTestEnv(t, func(_ *config.Env, s *config.Settings) {
		s.PublicBaseURL = "https://gateway.example.com"
	})
	req := httptest.NewRequest("GET", "http://internal:8787/v1/videos", nil)
	if got := e.srv.baseURL(req); got != "https://gateway.example.com" {
		t.Fatalf("baseURL = %q", got)
	}
}

func TestBaseURLFallsBackToRequest(t *testing.T) {
	e := newTestEnv(t, nil)
	req := httptest.NewRequest("GET", "http://internal:8787/v1/videos", nil)
	got := e.srv.baseURL(req)
	if !strings.Contains(got, "internal:8787") {
		t.Fatalf("baseURL = %q", got)
	}
}

func TestWriteOpenAIErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOpenAIError(rec, http.StatusBadRequest, "nope", "bad_request")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decode(t, rec.Body.Bytes())
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["message"] != "nope" || errObj["code"] != "bad_request" {
		t.Fatalf("error body = %v", body)
	}
	if errObj["type"] != "invalid_request_error" {
		t.Fatalf("error type = %v", errObj["type"])
	}
}

func TestTaskToVideoObjectShape(t *testing.T) {
	now := time.Now()
	task := &model.Task{
		ID: "video_x", Status: model.StatusSucceeded, Model: "minimax-h3",
		Ratio: "9:16", Duration: 6, CreatedAt: now, UpdatedAt: now,
		Prompt: "hello",
	}
	obj := taskToVideoObject(task, "https://gw.example.com")
	if obj["object"] != "video" || obj["id"] != "video_x" {
		t.Fatalf("object = %v", obj)
	}
	// The status vocabulary is the gateway's own; the studio UI depends on it.
	if obj["status"] != model.StatusSucceeded {
		t.Fatalf("status = %v", obj["status"])
	}
	if obj["progress"] != 100 {
		t.Fatalf("progress = %v", obj["progress"])
	}
	want := "https://gw.example.com/v1/videos/video_x/content"
	if obj["video_url"] != want || obj["url"] != want {
		t.Fatalf("urls = %v / %v", obj["video_url"], obj["url"])
	}
	if obj["completed_at"] == nil {
		t.Fatal("succeeded task must report completed_at")
	}

	// A queued task must not advertise a URL.
	queued := *task
	queued.Status = model.StatusQueued
	obj2 := taskToVideoObject(&queued, "https://gw.example.com")
	if obj2["status"] != model.StatusQueued {
		t.Fatalf("status = %v", obj2["status"])
	}
	if _, ok := obj2["video_url"]; ok {
		t.Fatalf("queued task exposed a url: %v", obj2)
	}
	if obj2["completed_at"] != nil {
		t.Fatalf("queued task reported completion: %v", obj2)
	}

	// A failed task surfaces the reason.
	failed := *task
	failed.Status = model.StatusFailed
	failed.Error = "boom"
	obj3 := taskToVideoObject(&failed, "https://gw.example.com")
	if obj3["failure_reason"] != "boom" {
		t.Fatalf("failure_reason = %v", obj3["failure_reason"])
	}
}

func TestFetchImageRejectsUnsupportedSchemes(t *testing.T) {
	e := newTestEnv(t, nil)
	req := httptest.NewRequest("POST", "/v1/videos", nil)
	for _, raw := range []string{"file:///etc/passwd", "ftp://example.com/a.jpg", "javascript:alert(1)", "just a string"} {
		if _, _, err := e.srv.fetchImage(req, raw); err == nil {
			t.Fatalf("%s should be rejected", raw)
		}
	}
}

func TestFetchImageAcceptsDataURL(t *testing.T) {
	e := newTestEnv(t, nil)
	req := httptest.NewRequest("POST", "/v1/videos", nil)
	raw := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n"))
	data, name, err := e.srv.fetchImage(req, raw)
	if err != nil {
		t.Fatalf("fetchImage: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty payload")
	}
	if !strings.HasSuffix(name, ".png") {
		t.Fatalf("filename = %q, want a .png name", name)
	}
}

func TestFetchImageAcceptsUnpaddedBase64(t *testing.T) {
	e := newTestEnv(t, nil)
	req := httptest.NewRequest("POST", "/v1/videos", nil)
	raw := "data:image/jpeg;base64," + base64.RawStdEncoding.EncodeToString(jpegBytes)
	data, _, err := e.srv.fetchImage(req, raw)
	if err != nil {
		t.Fatalf("fetchImage: %v", err)
	}
	if string(data) != string(jpegBytes) {
		t.Fatalf("payload = %q", data)
	}
}

func TestFetchImageFromHTTP(t *testing.T) {
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpegBytes)
	}))
	defer img.Close()

	e := newTestEnv(t, nil)
	req := httptest.NewRequest("POST", "/v1/videos", nil)
	data, name, err := e.srv.fetchImage(req, img.URL+"/nested/a.jpg")
	if err != nil {
		t.Fatalf("fetchImage: %v", err)
	}
	if string(data) != string(jpegBytes) {
		t.Fatalf("payload = %q", data)
	}
	if name != "a.jpg" {
		t.Fatalf("filename = %q", name)
	}
}

func TestFetchImageReportsHTTPFailures(t *testing.T) {
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer img.Close()

	e := newTestEnv(t, nil)
	req := httptest.NewRequest("POST", "/v1/videos", nil)
	if _, _, err := e.srv.fetchImage(req, img.URL+"/missing.jpg"); err == nil {
		t.Fatal("expected a 404 image fetch to fail")
	}
}

func TestReadJSONIsLenientAboutUnknownFields(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/videos", strings.NewReader(
		`{"image":"x","temperature":0.7,"unknown":{"nested":true}}`))
	var out map[string]any
	if err := readJSON(req, &out); err != nil {
		t.Fatalf("readJSON: %v", err)
	}
	if out["image"] != "x" {
		t.Fatalf("decoded = %v", out)
	}
}

func TestReadJSONRejectsOversizedBody(t *testing.T) {
	huge := `{"image":"` + strings.Repeat("a", 40<<20) + `"}`
	req := httptest.NewRequest("POST", "/v1/videos", strings.NewReader(huge))
	var out map[string]any
	if err := readJSON(req, &out); err == nil {
		t.Fatal("expected an oversized body to be rejected")
	}
}

func TestReadJSONRejectsMalformedBody(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/videos", strings.NewReader("{not json"))
	var out map[string]any
	if err := readJSON(req, &out); err == nil {
		t.Fatal("expected malformed JSON to be rejected")
	}
}

func TestExtractLastUserContent(t *testing.T) {
	// The OpenAI object form must be unwrapped to the bare URL.
	obj := chatMessage{Role: "user", Content: json.RawMessage(
		`[{"type":"text","text":"animate this"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,AAA"}}]`)}
	image, text := extractLastUserContent(obj)
	if image != "data:image/jpeg;base64,AAA" {
		t.Fatalf("image = %q", image)
	}
	if text != "animate this" {
		t.Fatalf("text = %q", text)
	}

	// The bare-string form must keep working too.
	str := chatMessage{Role: "user", Content: json.RawMessage(
		`[{"type":"image_url","image_url":"https://example.com/a.jpg"}]`)}
	image, text = extractLastUserContent(str)
	if image != "https://example.com/a.jpg" {
		t.Fatalf("image = %q", image)
	}
	if text != "" {
		t.Fatalf("text = %q", text)
	}

	// A plain string content carries only text.
	plain := chatMessage{Role: "user", Content: json.RawMessage(`"just text"`)}
	image, text = extractLastUserContent(plain)
	if text != "just text" || image != "" {
		t.Fatalf("image = %q text = %q", image, text)
	}

	// Multiple text parts are joined, and the first image wins.
	multi := chatMessage{Role: "user", Content: json.RawMessage(
		`[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"u1"}},{"type":"text","text":"b"},{"type":"image_url","image_url":{"url":"u2"}}]`)}
	image, text = extractLastUserContent(multi)
	if image != "u1" || text != "a b" {
		t.Fatalf("image = %q text = %q", image, text)
	}

	if image, text := extractLastUserContent(chatMessage{}); image != "" || text != "" {
		t.Fatalf("empty message = %q/%q", image, text)
	}
}

func TestLastUserMessage(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: json.RawMessage(`"be nice"`)},
		{Role: "user", Content: json.RawMessage(`"first"`)},
		{Role: "assistant", Content: json.RawMessage(`"ok"`)},
		{Role: "user", Content: json.RawMessage(`"second"`)},
		{Role: "assistant", Content: json.RawMessage(`"done"`)},
	}
	got, ok := lastUserMessage(msgs)
	if !ok || string(got.Content) != `"second"` {
		t.Fatalf("lastUserMessage = %+v ok=%v", got, ok)
	}
	if _, ok := lastUserMessage([]chatMessage{{Role: "system", Content: json.RawMessage(`"x"`)}}); ok {
		t.Fatal("expected no user message")
	}
}

func TestRecovererTurnsPanicsIntoFiveHundreds(t *testing.T) {
	e := newTestEnv(t, nil)
	h := e.srv.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestRequestLoggerRecordsStatus(t *testing.T) {
	e := newTestEnv(t, nil)
	h := e.srv.requestLogger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestClientIPHonoursTrustProxy(t *testing.T) {
	e := newTestEnv(t, func(_ *config.Env, s *config.Settings) { s.TrustProxy = true })
	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "10.0.0.9:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	if got := e.srv.clientIP(req); got != "203.0.113.7" {
		t.Fatalf("clientIP = %q", got)
	}

	// Without trust_proxy the header must be ignored.
	e2 := newTestEnv(t, func(_ *config.Env, s *config.Settings) { s.TrustProxy = false })
	req2 := httptest.NewRequest("GET", "/x", nil)
	req2.RemoteAddr = "10.0.0.9:1234"
	req2.Header.Set("X-Forwarded-For", "203.0.113.7")
	if got := e2.srv.clientIP(req2); got != "10.0.0.9" {
		t.Fatalf("clientIP = %q", got)
	}
}

var _ = fmt.Sprintf
var _ = os.Remove
