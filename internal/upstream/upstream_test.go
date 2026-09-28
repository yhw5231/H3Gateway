package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yhw5231/H3Gateway/internal/config"
	"github.com/yhw5231/H3Gateway/internal/identity"
	"github.com/yhw5231/H3Gateway/internal/model"
)

func testSettings(base string) func() config.Settings {
	s := config.DefaultSettings()
	s.UpstreamBase = base
	s.Normalize()
	return func() config.Settings { return s }
}

func TestSniffImageMIME(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0}
	png := []byte("\x89PNG\r\n\x1a\n")
	webp := append([]byte("RIFF\x00\x00\x00\x00"), []byte("WEBP")...)

	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"jpeg", jpeg, "image/jpeg"},
		{"png", png, "image/png"},
		{"webp", webp, "image/webp"},
		{"riff but not webp", append([]byte("RIFF\x00\x00\x00\x00"), []byte("WAVE")...), "application/octet-stream"},
		{"gif", []byte("GIF89a"), "application/octet-stream"},
		{"empty", nil, "application/octet-stream"},
		{"truncated", []byte{0xff, 0xd8}, "application/octet-stream"},
	}
	for _, tc := range cases {
		if got := SniffImageMIME(tc.data); got != tc.want {
			t.Fatalf("%s: SniffImageMIME = %q, want %q", tc.name, got, tc.want)
		}
	}
	if !ValidImage(jpeg) || ValidImage([]byte("nope")) {
		t.Fatal("ValidImage disagrees with SniffImageMIME")
	}
}

func TestErrorClassification(t *testing.T) {
	rate := &Error{Status: 429, Code: "rate_limit_error"}
	if !rate.IsRateLimit() || rate.IsAuthGate() || rate.IsRetryable() {
		t.Fatalf("rate limit misclassified: %+v", rate)
	}
	auth := &Error{Status: 401, Code: "login_required"}
	if !auth.IsAuthGate() || auth.IsRateLimit() || auth.IsRetryable() {
		t.Fatalf("auth gate misclassified: %+v", auth)
	}
	server := &Error{Status: 503, Code: "upstream_error"}
	if !server.IsRetryable() || server.IsRateLimit() || server.IsAuthGate() {
		t.Fatalf("5xx misclassified: %+v", server)
	}
	network := &Error{Status: 0, Code: "network_error"}
	if !network.IsRetryable() {
		t.Fatalf("network error should be retryable: %+v", network)
	}
	badRequest := &Error{Status: 400, Code: "bad_request_error"}
	if badRequest.IsRetryable() || badRequest.IsRateLimit() || badRequest.IsAuthGate() {
		t.Fatalf("4xx should fail fast: %+v", badRequest)
	}
}

func TestSubmitSendsExpectedFormAndHeaders(t *testing.T) {
	type captured struct {
		form   url.Values
		header http.Header
		part   string
		ct     string
	}
	got := make(chan captured, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		file, hdr, err := r.FormFile("image")
		if err != nil {
			t.Errorf("FormFile: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer file.Close()
		body, _ := io.ReadAll(file)
		got <- captured{
			form:   r.MultipartForm.Value,
			header: r.Header.Clone(),
			part:   string(body),
			ct:     hdr.Header.Get("Content-Type"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"task_id":"t1","access_token":"tok","status":"queued","remaining":1,"limit":2}`))
	}))
	defer srv.Close()

	c := New(testSettings(srv.URL))
	defer c.Close()

	ident := &identity.Identity{ClientID: "mmtrial_test", VisitorID: "mmguest_test", ForgedIP: "2001:db8::5", Family: model.FamilyIPv6}
	image := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10}
	res, err := c.Submit(context.Background(), SubmitOptions{
		Image: image, Filename: "ref.jpg", Prompt: "make it move", Ratio: "9:16", Duration: 10,
	}, ident, "")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.TaskID != "t1" || res.AccessToken != "tok" {
		t.Fatalf("unexpected result %+v", res)
	}
	if !res.RemainingKnown || res.Remaining != 1 {
		t.Fatalf("remaining not parsed: %+v", res)
	}

	c2 := <-got
	if c2.form.Get("client_id") != "mmtrial_test" {
		t.Fatalf("client_id = %q", c2.form.Get("client_id"))
	}
	if c2.form.Get("visitorId") != "mmguest_test" {
		t.Fatalf("visitorId = %q", c2.form.Get("visitorId"))
	}
	if c2.form.Get("duration") != "10" {
		t.Fatalf("duration = %q", c2.form.Get("duration"))
	}
	if c2.form.Get("ratio") != "9:16" {
		t.Fatalf("ratio = %q", c2.form.Get("ratio"))
	}
	if c2.form.Get("prompt") != "make it move" {
		t.Fatalf("prompt = %q", c2.form.Get("prompt"))
	}
	if c2.form.Get("channelCode") != "direct" {
		t.Fatalf("channelCode = %q", c2.form.Get("channelCode"))
	}
	// The forged header is what rotates the anonymous allowance.
	if c2.header.Get("X-Forwarded-For") != "2001:db8::5" {
		t.Fatalf("X-Forwarded-For = %q", c2.header.Get("X-Forwarded-For"))
	}
	if c2.header.Get("X-MiniMax-Trial-Client") != "mmtrial_test" {
		t.Fatalf("X-MiniMax-Trial-Client = %q", c2.header.Get("X-MiniMax-Trial-Client"))
	}
	if c2.ct != "image/jpeg" {
		t.Fatalf("image part content type = %q", c2.ct)
	}
	if c2.part != string(image) {
		t.Fatalf("image payload = %q", c2.part)
	}
}

func TestForgedHeaderOnlySentWithoutProxy(t *testing.T) {
	c := New(testSettings("https://siftq.com"))
	defer c.Close()
	ident := &identity.Identity{ClientID: "c", VisitorID: "v", ForgedIP: "2001:db8::1", Family: model.FamilyIPv6}

	direct := c.headers(ident, true)
	if direct["X-Forwarded-For"] != "2001:db8::1" {
		t.Fatalf("direct request must forge the header, got %q", direct["X-Forwarded-For"])
	}
	if direct["X-MiniMax-Trial-Client"] != "c" {
		t.Fatalf("client header = %q", direct["X-MiniMax-Trial-Client"])
	}

	// With a proxy the egress address is the honest quota key, so the header is
	// dropped rather than forged on top of it.
	viaProxy := c.headers(ident, false)
	if _, ok := viaProxy["X-Forwarded-For"]; ok {
		t.Fatalf("forged XFF must be omitted when a proxy supplies the real address: %q", viaProxy["X-Forwarded-For"])
	}

	// An identity with no forged address never sends the header either.
	none := c.headers(&identity.Identity{ClientID: "c", Family: model.FamilyNone}, true)
	if _, ok := none["X-Forwarded-For"]; ok {
		t.Fatal("family none must not send X-Forwarded-For")
	}
}

func TestSubmitWithoutRemainingFieldMarksItUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"task_id":"t1","access_token":"tok","status":"queued"}`))
	}))
	defer srv.Close()

	c := New(testSettings(srv.URL))
	defer c.Close()
	ident := &identity.Identity{ClientID: "c", VisitorID: "v", ForgedIP: "1.2.3.4"}
	res, err := c.Submit(context.Background(), SubmitOptions{Image: []byte{0xff, 0xd8, 0xff}}, ident, "")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.RemainingKnown {
		t.Fatal("a missing remaining field must not be treated as zero")
	}
}

func TestSubmitMapsUpstreamErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		check  func(*testing.T, error)
	}{
		{429, `{"type":"error","error":{"http_code":"429","type":"rate_limit_error","message":"used today"}}`,
			func(t *testing.T, err error) {
				var ue *Error
				if !asError(err, &ue) || !ue.IsRateLimit() {
					t.Fatalf("expected a rate limit error, got %v", err)
				}
			}},
		{401, `{"type":"error","error":{"http_code":"401","type":"login_required","message":"Sign in"}}`,
			func(t *testing.T, err error) {
				var ue *Error
				if !asError(err, &ue) || !ue.IsAuthGate() {
					t.Fatalf("expected an auth gate error, got %v", err)
				}
			}},
		{502, `<html>bad gateway</html>`,
			func(t *testing.T, err error) {
				var ue *Error
				if !asError(err, &ue) || !ue.IsRetryable() {
					t.Fatalf("expected a retryable error, got %v", err)
				}
			}},
		{415, `{"type":"error","error":{"http_code":"415","type":"bad_request_error","message":"Only JPG, PNG, or WEBP images are supported"}}`,
			func(t *testing.T, err error) {
				var ue *Error
				if !asError(err, &ue) || ue.IsRetryable() || ue.IsRateLimit() {
					t.Fatalf("expected a fail-fast error, got %v", err)
				}
				if !strings.Contains(ue.Message, "JPG") {
					t.Fatalf("message lost: %q", ue.Message)
				}
			}},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		c := New(testSettings(srv.URL))
		ident := &identity.Identity{ClientID: "c", VisitorID: "v", ForgedIP: "1.2.3.4"}
		_, err := c.Submit(context.Background(), SubmitOptions{Image: []byte{0xff, 0xd8, 0xff}}, ident, "")
		if err == nil {
			t.Fatalf("status %d: expected an error", tc.status)
		}
		tc.check(t, err)
		c.Close()
		srv.Close()
	}
}

func TestPollAndContentUseAccessToken(t *testing.T) {
	var pollQuery, contentQuery url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/api/minimax-trial/video-generation/t1", func(w http.ResponseWriter, r *http.Request) {
		pollQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"succeeded","task_id":"t1"}`))
	})
	mux.HandleFunc("/api/minimax-trial/video-generation/t1/content", func(w http.ResponseWriter, r *http.Request) {
		contentQuery = r.URL.Query()
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("MP4DATA"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(testSettings(srv.URL))
	defer c.Close()
	ident := &identity.Identity{ClientID: "cid", ForgedIP: "1.2.3.4"}

	res, err := c.Poll(context.Background(), "t1", "TOKEN", ident, "")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.Status != "succeeded" {
		t.Fatalf("status = %q", res.Status)
	}
	if pollQuery.Get("access_token") != "TOKEN" {
		t.Fatalf("poll access_token = %q", pollQuery.Get("access_token"))
	}

	data, media, err := c.Content(context.Background(), "t1", "TOKEN", ident, "")
	if err != nil {
		t.Fatalf("Content: %v", err)
	}
	if string(data) != "MP4DATA" || media != "video/mp4" {
		t.Fatalf("content = %q media = %q", data, media)
	}
	if contentQuery.Get("client_id") != "cid" || contentQuery.Get("access_token") != "TOKEN" {
		t.Fatalf("content query = %v", contentQuery)
	}
}

func TestQuotaParsesUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Forwarded-For"); got != "198.51.100.9" {
			t.Errorf("X-Forwarded-For = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"enabled":true,"limit":2,"used":1,"remaining":1,"max_concurrent":5}`))
	}))
	defer srv.Close()

	c := New(testSettings(srv.URL))
	defer c.Close()
	u, err := c.Quota(context.Background(), "198.51.100.9", "")
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if u.Limit != 2 || u.Used != 1 || u.Remaining != 1 || u.MaxConcurrent != 5 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestSubmitHonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	c := New(testSettings(srv.URL))
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	ident := &identity.Identity{ClientID: "c", VisitorID: "v", ForgedIP: "1.2.3.4"}
	_, err := c.Submit(ctx, SubmitOptions{Image: []byte{0xff, 0xd8, 0xff}}, ident, "")
	if err == nil {
		t.Fatal("expected the cancelled submit to fail")
	}
	var ue *Error
	if !asError(err, &ue) || ue.Code != "network_error" {
		t.Fatalf("expected a network_error, got %v", err)
	}
}

func TestPickProxyRotatesAndRespectsEmptyList(t *testing.T) {
	s := config.DefaultSettings()
	c := New(func() config.Settings { return s })
	defer c.Close()
	if got := c.PickProxy(); got != "" {
		t.Fatalf("expected no proxy, got %q", got)
	}
	s.ProxyList = []string{"http://a:1", "http://b:2"}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		seen[c.PickProxy()] = true
	}
	if len(seen) != 2 {
		t.Fatalf("proxy rotation did not use every entry: %v", seen)
	}
}

func TestHostOf(t *testing.T) {
	if got := hostOf("https://siftq.com/api/minimax-trial"); got != "siftq.com" {
		t.Fatalf("hostOf = %q", got)
	}
	if got := hostOf("://bad"); got != "siftq.com" {
		t.Fatalf("hostOf fallback = %q", got)
	}
}

// asError is a tiny local errors.As shim to keep the table test readable.
func asError(err error, target **Error) bool {
	if e, ok := err.(*Error); ok {
		*target = e
		return true
	}
	return false
}

var _ = json.Marshal
