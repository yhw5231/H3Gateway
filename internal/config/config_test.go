package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultSettingsNormalizeKeepsSaneValues(t *testing.T) {
	s := DefaultSettings()
	s.Normalize()
	if s.MaxConcurrent != 4 {
		t.Fatalf("max_concurrent = %d", s.MaxConcurrent)
	}
	if s.EndpointMode != EndpointShowcase {
		t.Fatalf("endpoint_mode = %q", s.EndpointMode)
	}
	if s.SourceHost != "siftq.com" {
		t.Fatalf("source_host = %q", s.SourceHost)
	}
	if s.XFFMode != XFFAuto {
		t.Fatalf("xff_mode = %q", s.XFFMode)
	}
	if s.XFFPool != XFFPoolPublic {
		t.Fatalf("xff_pool = %q", s.XFFPool)
	}
}

func TestNormalizeRepairsHostileInput(t *testing.T) {
	s := Settings{
		UpstreamBase:     "  siftq.com///  ",
		TrialBase:        "api/minimax-trial/",
		EndpointMode:     "nonsense",
		ShowcaseID:       "   ",
		SourceHost:       "  ",
		DefaultPrompt:    "",
		MaxConcurrent:    -5,
		SubmitTimeoutSec: 1,
		PollIntervalSec:  0,
		TaskResubmits:    -3,
		ImageMaxBytes:    1,
		XFFMode:          "wat",
		XFFPool:          "wat",
		TaskRetention:    1,
		ProxyList:        []string{"", "  ", "http://a:1", ""},
		PublicBaseURL:    "https://x.example.com/",
	}
	s.Normalize()

	if s.UpstreamBase != "https://siftq.com" {
		t.Fatalf("upstream_base = %q", s.UpstreamBase)
	}
	if s.TrialBase != "/api/minimax-trial" {
		t.Fatalf("trial_base = %q", s.TrialBase)
	}
	if s.EndpointMode != EndpointShowcase {
		t.Fatalf("endpoint_mode = %q", s.EndpointMode)
	}
	if s.ShowcaseID == "" || s.DefaultPrompt == "" {
		t.Fatal("blank showcase id / prompt should fall back to defaults")
	}
	if s.SourceHost != "siftq.com" {
		t.Fatalf("blank source_host should fall back to siftq.com, got %q", s.SourceHost)
	}
	if s.MaxConcurrent < 1 {
		t.Fatalf("max_concurrent = %d", s.MaxConcurrent)
	}
	if s.SubmitTimeoutSec < 5 {
		t.Fatalf("submit_timeout_sec = %d", s.SubmitTimeoutSec)
	}
	if s.PollIntervalSec < 0.5 {
		t.Fatalf("poll_interval_sec = %v", s.PollIntervalSec)
	}
	if s.TaskResubmits != 0 {
		t.Fatalf("task_resubmits = %d", s.TaskResubmits)
	}
	if s.ImageMaxBytes != DefaultSettings().ImageMaxBytes {
		t.Fatalf("image_max_bytes = %d", s.ImageMaxBytes)
	}
	if s.XFFMode != XFFAuto || s.XFFPool != XFFPoolPublic {
		t.Fatalf("xff mode/pool = %q/%q", s.XFFMode, s.XFFPool)
	}
	if len(s.ProxyList) != 1 || s.ProxyList[0] != "http://a:1" {
		t.Fatalf("proxy_list = %v", s.ProxyList)
	}
	if s.PublicBaseURL != "https://x.example.com" {
		t.Fatalf("public_base_url = %q", s.PublicBaseURL)
	}
}

func TestNormalizeAddsSchemeToBareHost(t *testing.T) {
	s := DefaultSettings()
	s.UpstreamBase = "example.com"
	s.Normalize()
	if s.UpstreamBase != "https://example.com" {
		t.Fatalf("upstream_base = %q", s.UpstreamBase)
	}
}

func TestValidateRejectsOutOfRangeValues(t *testing.T) {
	bad := []struct {
		name string
		mut  func(*Settings)
	}{
		{"max_concurrent", func(s *Settings) { s.MaxConcurrent = 999 }},
		{"submit_timeout", func(s *Settings) { s.SubmitTimeoutSec = 1 }},
		{"poll_interval", func(s *Settings) { s.PollIntervalSec = 900 }},
		{"endpoint_mode", func(s *Settings) { s.EndpointMode = "bogus" }},
		{"xff_mode", func(s *Settings) { s.XFFMode = "bogus" }},
	}
	for _, tc := range bad {
		s := DefaultSettings()
		tc.mut(&s)
		if err := s.Validate(); err == nil {
			t.Fatalf("%s: expected a validation error", tc.name)
		}
	}
	if err := DefaultSettings().Validate(); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}
}

func TestURLHelpers(t *testing.T) {
	s := DefaultSettings()
	s.UpstreamBase = "https://siftq.com"
	s.TrialBase = "/api/minimax-trial"
	if got := s.UsageURL(); got != "https://siftq.com/api/minimax-trial/usage" {
		t.Fatalf("usage url = %q", got)
	}
	// showcase is the default: it is the only endpoint that keeps the uploaded
	// picture for the whole clip.
	if got := s.GenerateURL(); got != "https://siftq.com/api/minimax-trial/showcase/video-generation" {
		t.Fatalf("default generate url = %q", got)
	}
	s.EndpointMode = EndpointPlain
	if got := s.GenerateURL(); got != "https://siftq.com/api/minimax-trial/video-generation" {
		t.Fatalf("plain generate url = %q", got)
	}
}

func TestSourceHostValue(t *testing.T) {
	s := DefaultSettings()
	if got := s.SourceHostValue(); got != "siftq.com" {
		t.Fatalf("source host = %q", got)
	}
	// An explicit value wins, so pointing UpstreamBase at a mirror or a local
	// mock cannot change what the gateway claims upstream.
	s.UpstreamBase = "http://127.0.0.1:8791"
	if got := s.SourceHostValue(); got != "siftq.com" {
		t.Fatalf("source host must not follow upstream_base, got %q", got)
	}
	// With no explicit value it degrades to the upstream host.
	s.SourceHost = ""
	if got := s.SourceHostValue(); got != "127.0.0.1:8791" {
		t.Fatalf("fallback source host = %q", got)
	}
	s.UpstreamBase = "https://mirror.example.com/api"
	if got := s.SourceHostValue(); got != "mirror.example.com" {
		t.Fatalf("fallback source host = %q", got)
	}
}

func TestCloneIsDeep(t *testing.T) {
	s := DefaultSettings()
	s.ProxyList = []string{"http://a:1"}
	c := s.Clone()
	c.ProxyList[0] = "http://b:2"
	if s.ProxyList[0] != "http://a:1" {
		t.Fatal("Clone shares the proxy slice with the original")
	}
}

func TestDurations(t *testing.T) {
	s := DefaultSettings()
	s.SubmitTimeoutSec = 900
	s.PollIntervalSec = 3
	if s.SubmitTimeout() != 900*time.Second {
		t.Fatalf("submit timeout = %v", s.SubmitTimeout())
	}
	if s.PollInterval() != 3*time.Second {
		t.Fatalf("poll interval = %v", s.PollInterval())
	}
}

func TestSettingsFromEnvReadsGatewayVariables(t *testing.T) {
	t.Setenv("GATEWAY_MAX_CONCURRENT", "7")
	t.Setenv("GATEWAY_XFF_MODE", "ipv6")
	t.Setenv("GATEWAY_XFF_POOL", "reserved")
	t.Setenv("GATEWAY_XFF_VARIANTS", "true")
	t.Setenv("PROXY_LIST", "socks5://127.0.0.1:1080, http://p:8080")
	t.Setenv("GATEWAY_ENDPOINT_MODE", "showcase")
	t.Setenv("GATEWAY_SOURCE_HOST", "siftq.com")

	s := SettingsFromEnv()
	if s.MaxConcurrent != 7 {
		t.Fatalf("max_concurrent = %d", s.MaxConcurrent)
	}
	if s.XFFMode != XFFIPv6 {
		t.Fatalf("xff_mode = %q", s.XFFMode)
	}
	if s.XFFPool != XFFPoolReserved {
		t.Fatalf("xff_pool = %q", s.XFFPool)
	}
	if !s.XFFVariants {
		t.Fatal("xff_variants should be true")
	}
	if len(s.ProxyList) != 2 {
		t.Fatalf("proxy_list = %v", s.ProxyList)
	}
	if s.EndpointMode != EndpointShowcase {
		t.Fatalf("endpoint_mode = %q", s.EndpointMode)
	}
}

func TestEnvNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range EnvNames {
		if seen[name] {
			t.Fatalf("duplicate environment variable name %q", name)
		}
		seen[name] = true
		if !strings.HasPrefix(name, "GATEWAY_") && name != "PROXY_LIST" {
			t.Fatalf("%q should be namespaced with GATEWAY_", name)
		}
	}
}

// TestEnvExampleOnlyUsesKnownVariables keeps the documented environment
// variables honest: a rename in the code without updating .env.example (or the
// reverse) fails here instead of silently doing nothing in production.
func TestEnvExampleOnlyUsesKnownVariables(t *testing.T) {
	path := filepath.Join("..", "..", ".env.example")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	known := map[string]bool{}
	for _, name := range EnvNames {
		known[name] = true
	}
	// Compose-only variables that are consumed by docker-compose.yml rather than
	// by the gateway itself.
	for _, name := range []string{"H3_PORT", "H3_IMAGE", "H3_CONTAINER", "H3_VOLUME", "H3_VERSION", "TZ"} {
		known[name] = true
	}

	var documented int
	for i, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, _, ok := strings.Cut(trimmed, "=")
		if !ok {
			t.Fatalf("%s:%d: expected KEY=value, got %q", path, i+1, trimmed)
		}
		key = strings.TrimSpace(key)
		documented++
		if !known[key] {
			t.Errorf("%s:%d: %q is documented but not read by the gateway", path, i+1, key)
		}
	}
	if documented < 15 {
		t.Fatalf("only %d variables documented; the example file looks truncated", documented)
	}
}

func TestLoadEnvDefaultsAndOverrides(t *testing.T) {
	t.Setenv("GATEWAY_HOST", "0.0.0.0")
	t.Setenv("GATEWAY_PORT", "9000")
	t.Setenv("GATEWAY_DATA_DIR", "/var/lib/h3")
	e := LoadEnv()
	if e.Host != "0.0.0.0" || e.Port != 9000 {
		t.Fatalf("host/port = %s/%d", e.Host, e.Port)
	}
	if !strings.HasPrefix(e.DBPath, filepath.FromSlash("/var/lib/h3")) {
		t.Fatalf("db path = %q", e.DBPath)
	}
	if filepath.Base(e.DBPath) != "h3gateway.json" {
		t.Fatalf("db file name = %q", e.DBPath)
	}
	if e.AdminUser != "admin" || e.AdminPassword != "admin" {
		t.Fatalf("bootstrap credentials = %q/%q", e.AdminUser, e.AdminPassword)
	}
}
