// Package model holds the persisted domain types shared by the store, the
// pipeline and the HTTP layers.
package model

import "time"

// Task lifecycle states. They mirror the OpenAI Sora-style video object so the
// gateway can stay drop-in compatible with clients that speak that dialect.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// IP family actually used for the forged X-Forwarded-For of a task.
const (
	FamilyNone = "none"
	FamilyIPv4 = "ipv4"
	FamilyIPv6 = "ipv6"
)

// Task is one image-to-video job tracked by the gateway.
type Task struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Model    string `json:"model"`
	Ratio    string `json:"ratio"`
	Duration int    `json:"duration"`
	Prompt   string `json:"prompt,omitempty"`

	UpstreamTaskID string `json:"upstream_task_id,omitempty"`
	AccessToken    string `json:"access_token,omitempty"`
	ClientID       string `json:"client_id,omitempty"`
	VisitorID      string `json:"visitor_id,omitempty"`

	// ForgedIP is the exact X-Forwarded-For value handed to upstream. Upstream
	// keys its anonymous quota on the raw first token of that header, so the
	// original string is replayed verbatim on poll/content requests.
	ForgedIP string `json:"forged_ip,omitempty"`
	IPFamily string `json:"ip_family,omitempty"`

	VideoURL string `json:"video_url,omitempty"`
	Error    string `json:"error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Attempts  int       `json:"attempts"`

	APIKeyID  string `json:"api_key_id,omitempty"`
	APIKeyTag string `json:"api_key_tag,omitempty"`
	Source    string `json:"source,omitempty"` // api | studio | admin
}

// Terminal reports whether no further upstream polling is needed.
func (t *Task) Terminal() bool {
	return t.Status == StatusSucceeded || t.Status == StatusFailed || t.Status == StatusCanceled
}

// APIKey is a bearer credential accepted on /v1/*.
type APIKey struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Key       string     `json:"key"`
	Enabled   bool       `json:"enabled"`
	Note      string     `json:"note,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`

	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	Requests   int64     `json:"requests"`

	// RateLimitPerMin caps requests per rolling minute for this key. Zero means
	// unlimited.
	RateLimitPerMin int `json:"rate_limit_per_min,omitempty"`
}

// Expired reports whether the key is past its optional expiry.
func (k *APIKey) Expired(now time.Time) bool {
	return k.ExpiresAt != nil && now.After(*k.ExpiresAt)
}

// AdminUser is a back-office account. Exactly one user is seeded on first run.
type AdminUser struct {
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	Salt         string    `json:"salt"`
	Iterations   int       `json:"iterations"`
	UpdatedAt    time.Time `json:"updated_at"`
	// MustChangePassword is set while the account still uses the bootstrap
	// credentials, so the UI can nag until it is rotated.
	MustChangePassword bool `json:"must_change_password,omitempty"`
}

// XFFProbeRecord caches the outcome of the live X-Forwarded-For capability
// test so the admin UI can show it without re-running the probe.
type XFFProbeRecord struct {
	RanAt        time.Time `json:"ran_at"`
	OK           bool      `json:"ok"`
	Upstream     string    `json:"upstream"`
	IPv4Accepted bool      `json:"ipv4_accepted"`
	IPv6Accepted bool      `json:"ipv6_accepted"`
	// DryRun marks a reachability-only run: the accepted flags are deliberately
	// false because no generation was submitted to prove quota keying.
	DryRun  bool     `json:"dry_run,omitempty"`
	Message string   `json:"message"`
	Steps   []string `json:"steps,omitempty"`
}
