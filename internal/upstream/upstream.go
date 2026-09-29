// Package upstream talks to the SiftQ MiniMax-H3 anonymous trial API.
//
// The wire contract was recovered from the site's own bundle and confirmed by
// live probing:
//
//	POST {trial}/video-generation              (plain trial channel, anonymous)
//	POST {trial}/showcase/video-generation     (showcase channel, now login-gated)
//	GET  {trial}/video-generation/{id}?access_token=...
//	GET  {trial}/video-generation/{id}/content?client_id=...&access_token=...
//	GET  {trial}/usage                         (quota for the current XFF key)
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yhw5231/H3Gateway/internal/auth"
	"github.com/yhw5231/H3Gateway/internal/config"
	"github.com/yhw5231/H3Gateway/internal/identity"
)

// Error is a structured upstream failure.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %d %s: %s", e.Status, e.Code, e.Message)
}

// IsRateLimit reports the "you used your free generations" answer.
func (e *Error) IsRateLimit() bool {
	return e.Status == 429 || e.Code == "rate_limit_error" || e.Code == "free_quota_exhausted"
}

// IsAuthGate reports the answers that mean "this identity must sign in".
func (e *Error) IsAuthGate() bool {
	return e.Status == 401 || e.Status == 403 ||
		e.Code == "login_required" || e.Code == "purchase_required" ||
		e.Code == "credits_required" || e.Code == "trial_disabled"
}

// IsRetryable reports transient upstream trouble worth backing off on.
func (e *Error) IsRetryable() bool {
	if e.Status >= 500 || e.Status == 0 {
		return true
	}
	switch e.Code {
	case "generation_retryable", "network_error":
		return true
	}
	return false
}

// SubmitResult is the accepted-task payload.
type SubmitResult struct {
	TaskID         string `json:"task_id"`
	AccessToken    string `json:"access_token"`
	Status         string `json:"status"`
	QueuePosition  int    `json:"queue_position"`
	ActiveCount    int    `json:"active_count"`
	MaxConcurrent  int    `json:"max_concurrent"`
	Authenticated  bool   `json:"authenticated"`
	Limit          int    `json:"limit"`
	Remaining      int    `json:"remaining"`
	BonusRemaining int    `json:"bonus_remaining"`
	// RemainingKnown distinguishes "server said 0 left" from "server said
	// nothing", because the absence of the field must not retire an identity.
	RemainingKnown bool `json:"-"`
}

// Usage is the quota view for one forged XFF key.
type Usage struct {
	Enabled                 bool `json:"enabled"`
	Authenticated           bool `json:"authenticated"`
	Limit                   int  `json:"limit"`
	Used                    int  `json:"used"`
	Remaining               int  `json:"remaining"`
	BonusRemaining          int  `json:"bonus_remaining"`
	AnonymousDailyLimit     int  `json:"anonymous_daily_limit"`
	AuthenticatedDailyLimit int  `json:"authenticated_daily_limit"`
	MaxConcurrent           int  `json:"max_concurrent"`
}

// PollResult is the task-status payload.
type PollResult struct {
	Status   string          `json:"status"`
	TaskID   string          `json:"task_id"`
	Error    string          `json:"error"`
	ErrorMsg string          `json:"error_message"`
	Message  string          `json:"message"`
	Raw      json.RawMessage `json:"-"`
}

// Client is a concurrency-safe upstream API client. HTTP clients are cached per
// proxy so connection pools are reused.
type Client struct {
	settings func() config.Settings

	mu      sync.Mutex
	clients map[string]*http.Client
}

// New builds a client bound to a settings provider.
func New(settings func() config.Settings) *Client {
	return &Client{settings: settings, clients: map[string]*http.Client{}}
}

// Close releases idle connections.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cl := range c.clients {
		if tr, ok := cl.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
	c.clients = map[string]*http.Client{}
}

func (c *Client) httpClient(proxy string) *http.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.clients[proxy]; ok {
		return cl
	}
	tr := &http.Transport{
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if proxy != "" {
		applyProxy(tr, proxy)
	}
	cl := &http.Client{
		Transport: tr,
		Timeout:   120 * time.Second,
		// The original never followed redirects; a redirect here would mean the
		// endpoint moved and the request must fail loudly instead.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	c.clients[proxy] = cl
	return cl
}

func applyProxy(tr *http.Transport, proxy string) {
	u, err := url.Parse(proxy)
	if err != nil {
		return
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		remoteDNS := strings.EqualFold(u.Scheme, "socks5h")
		tr.Proxy = nil
		tr.DialContext = socks5DialContext(u, remoteDNS)
	default:
		tr.Proxy = http.ProxyURL(u)
	}
}

// PickProxy chooses a proxy for one attempt, honouring the settings pool.
func (c *Client) PickProxy() string {
	s := c.settings()
	if len(s.ProxyList) == 0 {
		return ""
	}
	n := len(s.ProxyList)
	return s.ProxyList[rotatingIndex(n)]
}

func (c *Client) headers(ident *identity.Identity, forgedHeader bool) map[string]string {
	s := c.settings()
	h := map[string]string{
		"Accept":                 "application/json",
		"User-Agent":             s.UserAgent,
		"Referer":                s.Referer,
		"X-MiniMax-Trial-Client": ident.ClientID,
	}
	// With a real proxy the egress address is the honest quota key, so forging a
	// header on top of it would only make debugging harder.
	if forgedHeader && ident.ForgedIP != "" {
		h["X-Forwarded-For"] = ident.ForgedIP
	}
	return h
}

// SubmitOptions carries the per-request generation parameters.
type SubmitOptions struct {
	Image    []byte
	Filename string
	Prompt   string
	Ratio    string
	Duration int

	// IdempotencyKey identifies one logical generation. It must be unique per
	// generation: upstream answers a repeated key by replaying the task it
	// created the first time, which would hand the caller an older video. When
	// empty, a fresh key is minted per request.
	IdempotencyKey string
}

// NewIdempotencyKey mints the value for the Idempotency-Key request header.
//
// It is deliberately *not* derived from the identity: one forged address is
// reused for its second free generation, so an identity-derived key would make
// two different generations look like the same request.
func NewIdempotencyKey() string { return "mmtrial_" + auth.GenerateID() }

// Submit posts an image and returns the accepted upstream task.
func (c *Client) Submit(ctx context.Context, opt SubmitOptions, ident *identity.Identity, proxy string) (*SubmitResult, error) {
	s := c.settings()
	filename := opt.Filename
	if filename == "" {
		filename = "upload.jpg"
	}
	image := opt.Image
	if image == nil {
		return nil, &Error{Status: 0, Code: "bad_request", Message: "image is required"}
	}
	ratio := opt.Ratio
	if ratio == "" {
		ratio = "9:16"
	}
	duration := opt.Duration
	if duration <= 0 {
		duration = CurrentDuration
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	write := func(k, v string) { _ = mw.WriteField(k, v) }
	write("visitorId", ident.VisitorID)
	write("channelCode", "direct")
	write("sourceHost", hostOf(s.UpstreamBase))
	write("ratio", ratio)
	write("duration", strconv.Itoa(duration))
	write("client_id", ident.ClientID)
	if s.EndpointMode == config.EndpointShowcase {
		write("showcase_id", s.ShowcaseID)
	}
	// The plain trial channel ignores `prompt` server-side, but sending it is
	// harmless and keeps the showcase mode working when it is re-enabled.
	if strings.TrimSpace(opt.Prompt) != "" {
		write("prompt", opt.Prompt)
	}

	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image"; filename=%q`, filename))
	hdr.Set("Content-Type", SniffImageMIME(image))
	part, err := mw.CreatePart(hdr)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(image); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.GenerateURL(), bytes.NewReader(body.Bytes()))
	if err != nil {
		return nil, err
	}
	for k, v := range c.headers(ident, proxy == "") {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	idempotencyKey := strings.TrimSpace(opt.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = NewIdempotencyKey()
	}
	req.Header.Set("Idempotency-Key", idempotencyKey)

	raw, err := c.do(c.httpClient(proxy), req)
	if err != nil {
		return nil, &Error{Status: 0, Code: "network_error", Message: err.Error()}
	}
	if err := checkError(raw); err != nil {
		return nil, err
	}
	var res SubmitResult
	if err := json.Unmarshal(raw.body, &res); err != nil {
		return nil, &Error{Status: raw.status, Code: "decode_error", Message: truncate(string(raw.body), 200)}
	}
	res.RemainingKnown = raw.hasField("remaining")
	return &res, nil
}

// Poll fetches the status of an upstream task.
func (c *Client) Poll(ctx context.Context, taskID, accessToken string, ident *identity.Identity, proxy string) (*PollResult, error) {
	s := c.settings()
	u := s.TrialURL("/video-generation/" + url.PathEscape(taskID))
	if accessToken != "" {
		u += "?access_token=" + url.QueryEscape(accessToken)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range c.headers(ident, proxy == "") {
		req.Header.Set(k, v)
	}
	raw, err := c.do(c.httpClient(proxy), req)
	if err != nil {
		return nil, &Error{Status: 0, Code: "network_error", Message: err.Error()}
	}
	if err := checkError(raw); err != nil {
		return nil, err
	}
	var res PollResult
	if err := json.Unmarshal(raw.body, &res); err != nil {
		return nil, &Error{Status: raw.status, Code: "decode_error", Message: truncate(string(raw.body), 200)}
	}
	res.Raw = append(json.RawMessage(nil), raw.body...)
	return &res, nil
}

// Content streams down the produced MP4.
func (c *Client) Content(ctx context.Context, taskID, accessToken string, ident *identity.Identity, proxy string) ([]byte, string, error) {
	s := c.settings()
	u := s.TrialURL("/video-generation/" + url.PathEscape(taskID) + "/content")
	q := url.Values{}
	q.Set("client_id", ident.ClientID)
	if accessToken != "" {
		q.Set("access_token", accessToken)
	}
	u += "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	for k, v := range c.headers(ident, proxy == "") {
		req.Header.Set(k, v)
	}
	raw, err := c.do(c.httpClient(proxy), req)
	if err != nil {
		return nil, "", &Error{Status: 0, Code: "network_error", Message: err.Error()}
	}
	if err := checkError(raw); err != nil {
		return nil, "", err
	}
	media := raw.contentType
	if i := strings.IndexByte(media, ';'); i >= 0 {
		media = media[:i]
	}
	if media == "" {
		media = "video/mp4"
	}
	return raw.body, media, nil
}

// Quota checks the remaining anonymous allowance for one forged XFF value.
func (c *Client) Quota(ctx context.Context, xff, proxy string) (*Usage, error) {
	s := c.settings()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.UsageURL(), nil)
	if err != nil {
		return nil, err
	}
	ident := &identity.Identity{ClientID: "mmtrial_probe_" + identityProbeTag, VisitorID: "mmguest_probe", ForgedIP: xff}
	for k, v := range c.headers(ident, proxy == "") {
		req.Header.Set(k, v)
	}
	raw, err := c.do(c.httpClient(proxy), req)
	if err != nil {
		return nil, &Error{Status: 0, Code: "network_error", Message: err.Error()}
	}
	if err := checkError(raw); err != nil {
		return nil, err
	}
	var u Usage
	if err := json.Unmarshal(raw.body, &u); err != nil {
		return nil, &Error{Status: raw.status, Code: "decode_error", Message: truncate(string(raw.body), 200)}
	}
	return &u, nil
}

// Reachable performs a lightweight connectivity probe against the usage endpoint.
func (c *Client) Reachable(ctx context.Context) error {
	_, err := c.Quota(ctx, "", "")
	return err
}

// ---------------------------------------------------------------------------
// response plumbing
// ---------------------------------------------------------------------------

type rawResponse struct {
	status      int
	body        []byte
	contentType string
	errType     string
	errMessage  string
	fields      map[string]json.RawMessage
}

func (r *rawResponse) hasField(name string) bool {
	if r.fields == nil {
		return false
	}
	_, ok := r.fields[name]
	return ok
}

func (c *Client) do(cl *http.Client, req *http.Request) (*rawResponse, error) {
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	limit := int64(64 << 20) // MP4 downloads can be large
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err
	}
	raw := &rawResponse{status: resp.StatusCode, body: body, contentType: ct}
	if strings.Contains(ct, "json") {
		// Keep the top-level fields so callers can tell "field absent" apart from
		// "field present but zero".
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) == nil {
			raw.fields = fields
		}
		var envelope struct {
			Type  string `json:"type"`
			Error struct {
				HTTPCode string `json:"http_code"`
				Type     string `json:"type"`
				Message  string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &envelope) == nil {
			raw.errType = envelope.Error.Type
			raw.errMessage = envelope.Error.Message
			if raw.errType == "" {
				raw.errType = envelope.Type
			}
		}
	}
	return raw, nil
}

// checkError converts a non-2xx answer into a structured Error.
func checkError(r *rawResponse) error {
	if r.status >= 200 && r.status < 300 {
		return nil
	}
	msg := r.errMessage
	if msg == "" {
		msg = truncate(string(r.body), 300)
	}
	code := r.errType
	if code == "" {
		code = "upstream_error"
	}
	return &Error{Status: r.status, Code: code, Message: msg}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// CurrentDuration is the fixed trial duration. The upstream trial only exposes a
// single 6-second 768P profile; longer models are emulated by the gateway
// accepting the request and reporting the requested length.
const CurrentDuration = 6

// identityProbeTag keeps probe client ids recognisable in upstream logs.
var identityProbeTag = strconv.FormatInt(time.Now().UnixNano(), 36)

var proxyCounter struct {
	sync.Mutex
	n int
}

func rotatingIndex(n int) int {
	if n <= 1 {
		return 0
	}
	proxyCounter.Lock()
	defer proxyCounter.Unlock()
	proxyCounter.n = (proxyCounter.n + 1) % n
	return proxyCounter.n
}

// SniffImageMIME decides the multipart content type from the actual bytes.
// Upstream rejects a mislabelled part, so the filename extension must not be
// trusted.
func SniffImageMIME(data []byte) string {
	switch {
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg"
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// ValidImage reports whether the payload sniffs as a supported image.
func ValidImage(data []byte) bool {
	return SniffImageMIME(data) != "application/octet-stream"
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "siftq.com"
	}
	return u.Hostname()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// minimal SOCKS5 dialer (avoids golang.org/x/net/proxy)
// ---------------------------------------------------------------------------

func socks5DialContext(u *url.URL, remoteDNS bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	auth := ""
	if u.User != nil {
		auth = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			auth += "\x00" + pw
		}
	}
	target := u.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "1080")
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", target)
		if err != nil {
			return nil, fmt.Errorf("socks5 dial %s: %w", target, err)
		}
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
			defer conn.SetDeadline(time.Time{})
		}
		if err := socks5Handshake(conn, auth, addr, remoteDNS); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
}

func socks5Handshake(conn net.Conn, auth, addr string, remoteDNS bool) error {
	methods := []byte{0x00}
	if auth != "" {
		methods = append(methods, 0x02)
	}
	greeting := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if reply[0] != 0x05 {
		return fmt.Errorf("socks5: unexpected version %d", reply[0])
	}
	switch reply[1] {
	case 0x00:
	case 0x02:
		user, pass, _ := strings.Cut(auth, "\x00")
		msg := []byte{0x01, byte(len(user))}
		msg = append(msg, user...)
		msg = append(msg, byte(len(pass)))
		msg = append(msg, pass...)
		if _, err := conn.Write(msg); err != nil {
			return err
		}
		ar := make([]byte, 2)
		if _, err := io.ReadFull(conn, ar); err != nil {
			return err
		}
		if ar[1] != 0x00 {
			return fmt.Errorf("socks5: auth rejected")
		}
	default:
		return fmt.Errorf("socks5: no acceptable auth method (%d)", reply[1])
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return err
	}
	req := []byte{0x05, 0x01, 0x00}
	ip := net.ParseIP(host)
	switch {
	case !remoteDNS && ip != nil && ip.To4() != nil:
		req = append(req, 0x01)
		req = append(req, ip.To4()...)
	case !remoteDNS && ip != nil:
		req = append(req, 0x04)
		req = append(req, ip.To16()...)
	default:
		if len(host) > 255 {
			return fmt.Errorf("socks5: hostname too long")
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return err
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return err
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5: connect failed (code %d)", head[1])
	}
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return err
		}
		skip = int(l[0])
	case 0x04:
		skip = 16
	default:
		return fmt.Errorf("socks5: unknown address type %d", head[3])
	}
	_, err = io.ReadFull(conn, make([]byte, skip+2))
	return err
}
