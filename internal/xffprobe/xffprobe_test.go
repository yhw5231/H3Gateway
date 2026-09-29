package xffprobe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yhw5231/H3Gateway/internal/config"
	"github.com/yhw5231/H3Gateway/internal/upstream"
)

// serveGenerationOnBothPaths registers a generation handler for the plain and
// the showcase endpoint alike: the real service serves the same trial API under
// both prefixes, and the gateway submits to whichever one is configured.
func serveGenerationOnBothPaths(mux *http.ServeMux, h http.HandlerFunc) {
	mux.HandleFunc("/api/minimax-trial/video-generation", h)
	mux.HandleFunc("/api/minimax-trial/showcase/video-generation", h)
}

// fakeUpstream reproduces the quota behaviour that was observed on the real
// service: two anonymous generations per X-Forwarded-For value per day.
//
// ignoreIPv6 models the counterfactual in which the service parses only IPv4 and
// falls back to the real egress address for everything else. A probe that merely
// compared "before vs after" would wrongly call that IPv6 support; the control
// read must catch it.
type fakeUpstream struct {
	mu         sync.Mutex
	used       map[string]int
	ignoreIPv6 bool
	rejectAll  bool
}

func newFakeUpstream(ignoreIPv6 bool) *fakeUpstream {
	return &fakeUpstream{used: map[string]int{}, ignoreIPv6: ignoreIPv6}
}

const dailyLimit = 2

func (f *fakeUpstream) key(r *http.Request) string {
	xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if xff == "" {
		return "REAL-EGRESS"
	}
	if f.ignoreIPv6 {
		addr, err := netip.ParseAddr(xff)
		if err != nil || !addr.Is4() {
			return "REAL-EGRESS"
		}
	}
	return xff
}

func (f *fakeUpstream) remaining(k string) int {
	rem := dailyLimit - f.used[k]
	if rem < 0 {
		rem = 0
	}
	return rem
}

func (f *fakeUpstream) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/minimax-trial/usage", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		k := f.key(r)
		rem := f.remaining(k)
		writeJSON(w, map[string]any{
			"enabled": true, "authenticated": false,
			"limit": dailyLimit, "used": dailyLimit - rem, "remaining": rem,
			"anonymous_daily_limit": dailyLimit, "max_concurrent": 5,
		})
	})
	serveGenerationOnBothPaths(mux, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.rejectAll {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"http_code":"401","type":"login_required","message":"Sign in"}}`))
			return
		}
		k := f.key(r)
		if f.remaining(k) <= 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"http_code":"429","type":"rate_limit_error","message":"used today"}}`))
			return
		}
		f.used[k]++
		writeJSON(w, map[string]any{
			"task_id": "task-" + strconv.Itoa(f.used[k]), "access_token": "tok",
			"status": "queued", "limit": dailyLimit, "remaining": f.remaining(k),
		})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// testImage is a minimal but valid JPEG header; the fake upstream does not care
// about the pixels, only that a payload was sent.
var testImage = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}

func runProbeAgainst(t *testing.T, fake *fakeUpstream) *Result {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	settings := config.DefaultSettings()
	settings.UpstreamBase = srv.URL
	settings.Normalize()

	up := upstream.New(func() config.Settings { return settings })
	defer up.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return Run(ctx, up, settings, Options{Image: testImage, Filename: "probe.jpg"})
}

func TestProbeDetectsIPv4AndIPv6Support(t *testing.T) {
	res := runProbeAgainst(t, newFakeUpstream(false))
	if !res.OK {
		t.Fatalf("probe reported unreachable upstream: %s", res.Message)
	}
	if !res.IPv4Accepted {
		t.Fatalf("IPv4 XFF not detected as a quota key; steps: %v", res.Steps)
	}
	if !res.IPv6Accepted {
		t.Fatalf("IPv6 XFF not detected as a quota key; steps: %v", res.Steps)
	}
	if !strings.Contains(res.Message, "IPv6") {
		t.Fatalf("verdict message should mention IPv6: %q", res.Message)
	}
}

func TestProbeRejectsIPv6WhenUpstreamFallsBackToRealIP(t *testing.T) {
	res := runProbeAgainst(t, newFakeUpstream(true))
	if !res.IPv4Accepted {
		t.Fatalf("IPv4 should still be detected; steps: %v", res.Steps)
	}
	if res.IPv6Accepted {
		t.Fatalf("IPv6 was wrongly reported as supported; steps: %v", res.Steps)
	}
}

func TestProbeDryRunConsumesNothing(t *testing.T) {
	fake := newFakeUpstream(false)
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	settings := config.DefaultSettings()
	settings.UpstreamBase = srv.URL
	settings.Normalize()
	up := upstream.New(func() config.Settings { return settings })
	defer up.Close()

	res := Run(context.Background(), up, settings, Options{DryRun: true})
	if !res.OK {
		t.Fatalf("dry run reported unreachable upstream: %s", res.Message)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.used) != 0 {
		t.Fatalf("dry run consumed quota: %v", fake.used)
	}
}

func TestProbeReportsUnreachableUpstream(t *testing.T) {
	settings := config.DefaultSettings()
	// Port 1 on loopback refuses connections immediately.
	settings.UpstreamBase = "http://127.0.0.1:1"
	settings.Normalize()
	up := upstream.New(func() config.Settings { return settings })
	defer up.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res := Run(ctx, up, settings, Options{Image: testImage})
	if res.OK {
		t.Fatal("expected the probe to report the upstream as unreachable")
	}
	if res.Message == "" {
		t.Fatal("expected an explanatory message")
	}
}
