// Package config resolves process configuration from environment variables and
// holds the runtime-editable settings that the admin console can override.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Environment variable names. They deliberately keep the GATEWAY_* names used by
// the Python original so existing deployments only need to swap the binary.
const (
	EnvHost          = "GATEWAY_HOST"
	EnvPort          = "GATEWAY_PORT"
	EnvDataDir       = "GATEWAY_DATA_DIR"
	EnvDB            = "GATEWAY_DB"
	EnvVideosDir     = "GATEWAY_VIDEOS_DIR"
	EnvUploadsDir    = "GATEWAY_UPLOADS_DIR"
	EnvAdminUser     = "GATEWAY_ADMIN_USER"
	EnvAdminPassword = "GATEWAY_ADMIN_PASSWORD"
	EnvSessionSecret = "GATEWAY_SESSION_SECRET"
	EnvLogLevel      = "GATEWAY_LOG_LEVEL"

	EnvUpstream     = "GATEWAY_UPSTREAM"
	EnvTrialBase    = "GATEWAY_TRIAL_BASE"
	EnvEndpointMode = "GATEWAY_ENDPOINT_MODE"
	EnvShowcaseID   = "GATEWAY_SHOWCASE_ID"
	EnvDefPrompt    = "GATEWAY_DEFAULT_PROMPT"
	EnvMaxConc      = "GATEWAY_MAX_CONCURRENT"
	EnvSubmitTO     = "GATEWAY_SUBMIT_TIMEOUT"
	EnvPollInterval = "GATEWAY_POLL_INTERVAL"
	EnvResubmits    = "GATEWAY_TASK_RESUBMITS"
	EnvResubmitWait = "GATEWAY_RESUBMIT_BACKOFF"
	EnvImageMax     = "GATEWAY_IMAGE_MAX_BYTES"
	EnvProxyList    = "PROXY_LIST"

	EnvXFFMode     = "GATEWAY_XFF_MODE"
	EnvXFFVariants = "GATEWAY_XFF_VARIANTS"
	EnvXFFPool     = "GATEWAY_XFF_POOL"

	EnvAPIKey      = "GATEWAY_API_KEY"
	EnvRequireKey  = "GATEWAY_REQUIRE_API_KEY"
	EnvStudioSess  = "GATEWAY_STUDIO_SESSION"
	EnvRetention   = "GATEWAY_TASK_RETENTION"
	EnvPublicURL   = "GATEWAY_PUBLIC_URL"
	EnvTrustProxy  = "GATEWAY_TRUST_PROXY"
	EnvAdminListen = "GATEWAY_ADMIN_LISTEN"
)

// EnvNames lists every environment variable the gateway reads. It exists so the
// documented variables and the code cannot drift apart; see
// TestEnvExampleOnlyUsesKnownVariables.
var EnvNames = []string{
	EnvHost, EnvPort, EnvDataDir, EnvDB, EnvVideosDir, EnvUploadsDir,
	EnvAdminUser, EnvAdminPassword, EnvSessionSecret, EnvLogLevel,
	EnvUpstream, EnvTrialBase, EnvEndpointMode, EnvShowcaseID, EnvDefPrompt,
	EnvMaxConc, EnvSubmitTO, EnvPollInterval, EnvResubmits, EnvResubmitWait,
	EnvImageMax, EnvProxyList,
	EnvXFFMode, EnvXFFVariants, EnvXFFPool,
	EnvAPIKey, EnvRequireKey, EnvStudioSess, EnvRetention, EnvPublicURL,
	EnvTrustProxy, EnvAdminListen,
}

// Endpoint modes for the anonymous trial channel.
const (
	// EndpointPlain is /video-generation. It is anonymously usable and honours
	// ratio/duration but ignores `prompt` upstream.
	EndpointPlain = "plain"
	// EndpointShowcase is /showcase/video-generation. It used to honour `prompt`,
	// but upstream now answers it with login_required for anonymous callers.
	EndpointShowcase = "showcase"
)

// X-Forwarded-For forging modes.
const (
	XFFOff   = "off"
	XFFIPv4  = "ipv4"
	XFFIPv6  = "ipv6"
	XFFMixed = "mixed"
	// XFFAuto resolves to ipv6 when the last capability probe proved IPv6 works,
	// otherwise ipv4.
	XFFAuto = "auto"
)

// XFFPool* select which address space the forged X-Forwarded-For values are
// drawn from.
const (
	// XFFPoolPublic mints globally routable addresses, matching the original
	// Python gateway. Upstream accepts them and counts two generations per
	// address per day.
	XFFPoolPublic = "public"
	// XFFPoolReserved mints addresses from the RFC 5737 / RFC 3849
	// documentation ranges. Verified accepted by upstream, and no real client
	// shares those quota buckets.
	XFFPoolReserved = "reserved"
)

// Env is the process-level configuration that cannot be changed at runtime.
type Env struct {
	Host       string
	Port       int
	DataDir    string
	DBPath     string
	VideosDir  string
	UploadsDir string

	AdminUser     string
	AdminPassword string
	SessionSecret string
	LogLevel      string

	// BootstrapAPIKey is seeded into the key store on first run when set.
	BootstrapAPIKey string
}

// Settings are the operational knobs the admin console may change at runtime.
// Zero values are replaced by DefaultSettings on load, so it is always safe to
// persist a partial JSON document.
type Settings struct {
	UpstreamBase  string `json:"upstream_base"`
	TrialBase     string `json:"trial_base"`
	EndpointMode  string `json:"endpoint_mode"`
	ShowcaseID    string `json:"showcase_id"`
	DefaultPrompt string `json:"default_prompt"`
	UserAgent     string `json:"user_agent"`
	Referer       string `json:"referer"`

	MaxConcurrent    int     `json:"max_concurrent"`
	SubmitTimeoutSec int     `json:"submit_timeout_sec"`
	PollIntervalSec  float64 `json:"poll_interval_sec"`
	TaskResubmits    int     `json:"task_resubmits"`
	// ResubmitBackoffSec is the base delay before re-posting a task upstream
	// dropped. The delay grows with each attempt, up to five times the base.
	ResubmitBackoffSec int      `json:"resubmit_backoff_sec"`
	ImageMaxBytes      int64    `json:"image_max_bytes"`
	ProxyList          []string `json:"proxy_list"`

	XFFMode     string `json:"xff_mode"`
	XFFVariants bool   `json:"xff_variants"`
	XFFPool     string `json:"xff_pool"`

	RequireAPIKey        bool `json:"require_api_key"`
	StudioSessionEnabled bool `json:"studio_session_enabled"`
	TaskRetention        int  `json:"task_retention"`

	PublicBaseURL string `json:"public_base_url"`
	TrustProxy    bool   `json:"trust_proxy"`
}

// DefaultSettings mirrors the tuned defaults of the Python gateway.
func DefaultSettings() Settings {
	return Settings{
		UpstreamBase:  "https://siftq.com",
		TrialBase:     "/api/minimax-trial",
		EndpointMode:  EndpointPlain,
		ShowcaseID:    "case-mtqzygu8",
		DefaultPrompt: "Animate the scene in the image with natural, faithful motion. Keep the subject and setting consistent with the input image.",
		UserAgent:     "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		Referer:       "https://siftq.com/minimax-h3/try/zh",

		MaxConcurrent:      4,
		SubmitTimeoutSec:   900,
		PollIntervalSec:    3,
		TaskResubmits:      8,
		ResubmitBackoffSec: 30,
		ImageMaxBytes:      20 * 1024 * 1024,

		XFFMode:     XFFAuto,
		XFFVariants: false,
		XFFPool:     XFFPoolPublic,

		RequireAPIKey:        false,
		StudioSessionEnabled: true,
		TaskRetention:        2000,
	}
}

// Normalize repairs out-of-range or unknown values coming from a persisted
// settings document or from an admin request.
func (s *Settings) Normalize() {
	d := DefaultSettings()
	if strings.TrimSpace(s.UpstreamBase) == "" {
		s.UpstreamBase = d.UpstreamBase
	}
	s.UpstreamBase = strings.TrimRight(strings.TrimSpace(s.UpstreamBase), "/")
	if !strings.HasPrefix(s.UpstreamBase, "http://") && !strings.HasPrefix(s.UpstreamBase, "https://") {
		s.UpstreamBase = "https://" + s.UpstreamBase
	}
	if strings.TrimSpace(s.TrialBase) == "" {
		s.TrialBase = d.TrialBase
	}
	s.TrialBase = "/" + strings.Trim(strings.TrimSpace(s.TrialBase), "/")
	switch s.EndpointMode {
	case EndpointPlain, EndpointShowcase:
	default:
		s.EndpointMode = d.EndpointMode
	}
	if strings.TrimSpace(s.ShowcaseID) == "" {
		s.ShowcaseID = d.ShowcaseID
	}
	if strings.TrimSpace(s.DefaultPrompt) == "" {
		s.DefaultPrompt = d.DefaultPrompt
	}
	if strings.TrimSpace(s.UserAgent) == "" {
		s.UserAgent = d.UserAgent
	}
	if strings.TrimSpace(s.Referer) == "" {
		s.Referer = d.Referer
	}

	s.MaxConcurrent = clampInt(s.MaxConcurrent, 1, 64, d.MaxConcurrent)
	s.SubmitTimeoutSec = clampInt(s.SubmitTimeoutSec, 5, 86400, d.SubmitTimeoutSec)
	if s.PollIntervalSec < 0.5 {
		s.PollIntervalSec = d.PollIntervalSec
	}
	if s.PollIntervalSec > 120 {
		s.PollIntervalSec = 120
	}
	if s.TaskResubmits < 0 {
		s.TaskResubmits = 0
	}
	if s.TaskResubmits > 100 {
		s.TaskResubmits = 100
	}
	if s.ResubmitBackoffSec < 1 {
		s.ResubmitBackoffSec = d.ResubmitBackoffSec
	}
	if s.ResubmitBackoffSec > 900 {
		s.ResubmitBackoffSec = 900
	}
	if s.ImageMaxBytes < 64*1024 {
		s.ImageMaxBytes = d.ImageMaxBytes
	}
	if s.ImageMaxBytes > 512*1024*1024 {
		s.ImageMaxBytes = 512 * 1024 * 1024
	}

	clean := s.ProxyList[:0]
	for _, p := range s.ProxyList {
		if p = strings.TrimSpace(p); p != "" {
			clean = append(clean, p)
		}
	}
	s.ProxyList = clean

	switch s.XFFMode {
	case XFFOff, XFFIPv4, XFFIPv6, XFFMixed, XFFAuto:
	default:
		s.XFFMode = d.XFFMode
	}

	switch s.XFFPool {
	case XFFPoolPublic, XFFPoolReserved:
	default:
		s.XFFPool = d.XFFPool
	}

	if s.TaskRetention < 50 {
		s.TaskRetention = d.TaskRetention
	}
	if s.TaskRetention > 100000 {
		s.TaskRetention = 100000
	}
	s.PublicBaseURL = strings.TrimRight(strings.TrimSpace(s.PublicBaseURL), "/")
}

// Clone returns a deep copy so callers cannot mutate shared settings.
func (s Settings) Clone() Settings {
	c := s
	c.ProxyList = append([]string(nil), s.ProxyList...)
	return c
}

// SubmitTimeout returns the retry deadline as a duration.
func (s Settings) SubmitTimeout() time.Duration {
	return time.Duration(s.SubmitTimeoutSec) * time.Second
}

// PollInterval returns the upstream polling cadence.
func (s Settings) PollInterval() time.Duration {
	return time.Duration(s.PollIntervalSec * float64(time.Second))
}

// TrialURL joins the upstream base with the trial prefix and a sub path.
func (s Settings) TrialURL(sub string) string {
	return s.UpstreamBase + s.TrialBase + sub
}

// GenerateURL returns the generation endpoint implied by EndpointMode.
func (s Settings) GenerateURL() string {
	if s.EndpointMode == EndpointShowcase {
		return s.TrialURL("/showcase/video-generation")
	}
	return s.TrialURL("/video-generation")
}

// UsageURL returns the anonymous quota endpoint.
func (s Settings) UsageURL() string { return s.TrialURL("/usage") }

// LoadEnv reads the process environment. It never fails: unusable values fall
// back to defaults so a container with a typo still starts and reports the
// problem through the admin console.
func LoadEnv() Env {
	dataDir := envStr(EnvDataDir, "./data")
	e := Env{
		Host:      envStr(EnvHost, "127.0.0.1"),
		Port:      envInt(EnvPort, 8787),
		DataDir:   dataDir,
		AdminUser: envStr(EnvAdminUser, "admin"),
		// The default password is intentionally the well-known bootstrap value
		// requested for the back office; the console forces a rotation.
		AdminPassword:   envStr(EnvAdminPassword, "admin"),
		SessionSecret:   envStr(EnvSessionSecret, ""),
		LogLevel:        envStr(EnvLogLevel, "info"),
		BootstrapAPIKey: strings.TrimSpace(os.Getenv(EnvAPIKey)),
	}
	e.DBPath = envStr(EnvDB, filepath.Join(dataDir, "h3gateway.json"))
	e.VideosDir = envStr(EnvVideosDir, filepath.Join(dataDir, "videos"))
	e.UploadsDir = envStr(EnvUploadsDir, filepath.Join(dataDir, "uploads"))
	return e
}

// SettingsFromEnv builds the runtime settings that env variables seed before the
// persisted overrides are applied.
func SettingsFromEnv() Settings {
	s := DefaultSettings()
	s.UpstreamBase = envStr(EnvUpstream, s.UpstreamBase)
	s.TrialBase = envStr(EnvTrialBase, s.TrialBase)
	s.EndpointMode = envStr(EnvEndpointMode, s.EndpointMode)
	s.ShowcaseID = envStr(EnvShowcaseID, s.ShowcaseID)
	s.DefaultPrompt = envStr(EnvDefPrompt, s.DefaultPrompt)
	s.MaxConcurrent = envInt(EnvMaxConc, s.MaxConcurrent)
	s.SubmitTimeoutSec = envInt(EnvSubmitTO, s.SubmitTimeoutSec)
	s.PollIntervalSec = envFloat(EnvPollInterval, s.PollIntervalSec)
	s.TaskResubmits = envInt(EnvResubmits, s.TaskResubmits)
	s.ResubmitBackoffSec = envInt(EnvResubmitWait, s.ResubmitBackoffSec)
	s.ImageMaxBytes = int64(envInt(EnvImageMax, int(s.ImageMaxBytes)))
	s.ProxyList = splitList(envStr(EnvProxyList, ""))
	s.XFFMode = envStr(EnvXFFMode, s.XFFMode)
	s.XFFVariants = envBool(EnvXFFVariants, s.XFFVariants)
	s.XFFPool = envStr(EnvXFFPool, s.XFFPool)
	s.RequireAPIKey = envBool(EnvRequireKey, s.RequireAPIKey)
	s.StudioSessionEnabled = envBool(EnvStudioSess, s.StudioSessionEnabled)
	s.TaskRetention = envInt(EnvRetention, s.TaskRetention)
	s.PublicBaseURL = envStr(EnvPublicURL, "")
	s.TrustProxy = envBool(EnvTrustProxy, false)
	s.Normalize()
	return s
}

// Validate checks settings submitted from the admin console.
// ValidateInput rejects values a caller explicitly supplied that fall outside
// the accepted range. Zero values mean "unset" and are filled in by Normalize,
// so they pass here. This is what the admin console hits: Normalize would
// silently clamp a typo, which hides operator mistakes.
func (s Settings) ValidateInput() error {
	if s.MaxConcurrent < 0 || s.MaxConcurrent > 64 {
		return fmt.Errorf("max_concurrent must be between 1 and 64")
	}
	if s.SubmitTimeoutSec != 0 && (s.SubmitTimeoutSec < 5 || s.SubmitTimeoutSec > 86400) {
		return fmt.Errorf("submit_timeout_sec must be between 5 and 86400")
	}
	if s.PollIntervalSec != 0 && (s.PollIntervalSec < 0.5 || s.PollIntervalSec > 120) {
		return fmt.Errorf("poll_interval_sec must be between 0.5 and 120")
	}
	if s.TaskResubmits < 0 || s.TaskResubmits > 100 {
		return fmt.Errorf("task_resubmits must be between 0 and 100")
	}
	if s.ResubmitBackoffSec < 0 || s.ResubmitBackoffSec > 900 {
		return fmt.Errorf("resubmit_backoff_sec must be between 1 and 900")
	}
	if s.TaskRetention != 0 && (s.TaskRetention < 50 || s.TaskRetention > 100000) {
		return fmt.Errorf("task_retention must be between 50 and 100000")
	}
	if s.ImageMaxBytes != 0 && (s.ImageMaxBytes < 64*1024 || s.ImageMaxBytes > 512*1024*1024) {
		return fmt.Errorf("image_max_bytes must be between 64KiB and 512MiB")
	}
	switch s.EndpointMode {
	case "", EndpointPlain, EndpointShowcase:
	default:
		return fmt.Errorf("endpoint_mode must be %q or %q", EndpointPlain, EndpointShowcase)
	}
	switch s.XFFMode {
	case "", XFFOff, XFFIPv4, XFFIPv6, XFFMixed, XFFAuto:
	default:
		return fmt.Errorf("xff_mode must be one of off, ipv4, ipv6, mixed, auto")
	}
	switch s.XFFPool {
	case "", XFFPoolPublic, XFFPoolReserved:
	default:
		return fmt.Errorf("xff_pool must be %q or %q", XFFPoolPublic, XFFPoolReserved)
	}
	return nil
}

func (s Settings) Validate() error {
	if s.MaxConcurrent < 1 || s.MaxConcurrent > 64 {
		return fmt.Errorf("max_concurrent must be between 1 and 64")
	}
	if s.SubmitTimeoutSec < 5 || s.SubmitTimeoutSec > 86400 {
		return fmt.Errorf("submit_timeout_sec must be between 5 and 86400")
	}
	if s.PollIntervalSec < 0.5 || s.PollIntervalSec > 120 {
		return fmt.Errorf("poll_interval_sec must be between 0.5 and 120")
	}
	if s.EndpointMode != EndpointPlain && s.EndpointMode != EndpointShowcase {
		return fmt.Errorf("endpoint_mode must be %q or %q", EndpointPlain, EndpointShowcase)
	}
	switch s.XFFMode {
	case XFFOff, XFFIPv4, XFFIPv6, XFFMixed, XFFAuto:
	default:
		return fmt.Errorf("xff_mode must be one of off, ipv4, ipv6, mixed, auto")
	}
	return nil
}

// ---------------------------------------------------------------------------
// env helpers
// ---------------------------------------------------------------------------

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func clampInt(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
