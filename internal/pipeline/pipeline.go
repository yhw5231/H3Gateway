// Package pipeline owns the task lifecycle: submitting to upstream with identity
// rotation, polling until a terminal state, transparently resubmitting when
// upstream drops a task, and caching finished MP4s on disk.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yhw5231/H3Gateway/internal/config"
	"github.com/yhw5231/H3Gateway/internal/identity"
	"github.com/yhw5231/H3Gateway/internal/model"
	"github.com/yhw5231/H3Gateway/internal/store"
	"github.com/yhw5231/H3Gateway/internal/upstream"
)

// ErrUpstreamBusy means the submit window closed without success.
var ErrUpstreamBusy = errors.New("upstream submit failed")

// ErrImageRejected means the payload is not a usable image.
var ErrImageRejected = errors.New("image rejected")

// SubmitRequest describes one generation request.
type SubmitRequest struct {
	Image     []byte
	Filename  string
	Prompt    string
	Model     string
	Ratio     string
	Duration  int
	Source    string
	APIKeyID  string
	APIKeyTag string
}

// Pipeline coordinates the store, the rotator and the upstream client.
type Pipeline struct {
	store   *store.Store
	up      *upstream.Client
	rot     *identity.Rotator
	log     *slog.Logger
	probeFn func() *model.XFFProbeRecord

	videosDir  string
	uploadsDir string

	sem *dynSem

	mu      sync.Mutex
	running map[string]context.CancelFunc

	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

// New wires a pipeline together.
func New(st *store.Store, up *upstream.Client, rot *identity.Rotator, log *slog.Logger,
	videosDir, uploadsDir string, probeFn func() *model.XFFProbeRecord) *Pipeline {

	if log == nil {
		log = slog.Default()
	}
	if probeFn == nil {
		probeFn = func() *model.XFFProbeRecord { return nil }
	}
	for _, d := range []string{videosDir, uploadsDir} {
		_ = os.MkdirAll(d, 0o755)
	}
	p := &Pipeline{
		store:      st,
		up:         up,
		rot:        rot,
		log:        log,
		probeFn:    probeFn,
		videosDir:  videosDir,
		uploadsDir: uploadsDir,
		sem:        newDynSem(st.Settings().MaxConcurrent),
		running:    map[string]context.CancelFunc{},
	}
	// A poller must be startable even before Start is called, so the base
	// context exists from construction time.
	p.ctx, p.cancel = context.WithCancel(context.Background())
	// Retention pruning must not leave orphaned media behind.
	st.OnTaskRemoved = func(t *model.Task) {
		_ = os.Remove(p.VideoPath(t.ID))
		_ = os.Remove(p.UploadPath(t.ID))
	}
	return p
}

// Start re-bases the pollers onto ctx and resumes unfinished tasks. It returns
// immediately.
func (p *Pipeline) Start(ctx context.Context) {
	p.mu.Lock()
	previous := p.cancel
	p.ctx, p.cancel = context.WithCancel(ctx)
	p.mu.Unlock()
	// Release the placeholder context created by New, if it is still the base.
	if previous != nil {
		previous()
	}

	for _, t := range p.store.PendingTasks() {
		p.log.Info("resuming task after restart", "task", t.ID, "status", t.Status)
		p.startPoll(t)
	}
}

// Stop cancels every poller and waits for them to finish.
func (p *Pipeline) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	p.wg.Wait()
}

// Paths ---------------------------------------------------------------------

// VideoPath is where the finished MP4 for a task is cached.
func (p *Pipeline) VideoPath(taskID string) string {
	return filepath.Join(p.videosDir, taskID+".mp4")
}

// UploadPath is where the submitted input image is kept for resubmits.
func (p *Pipeline) UploadPath(taskID string) string {
	return filepath.Join(p.uploadsDir, taskID+".bin")
}

// Stats ---------------------------------------------------------------------

// Stats is a snapshot for the admin dashboard.
type Stats struct {
	Rotator       identity.Stats `json:"rotator"`
	InFlight      int            `json:"in_flight"`
	MaxConcurrent int            `json:"max_concurrent"`
}

// Stats reports pipeline counters.
func (p *Pipeline) Stats() Stats {
	s := p.rot.Stats()
	settings := p.store.Settings()
	effective := identity.EffectiveMode(settings, p.probeFn())
	s.EffectiveXFFMode = effective
	s.ForgedHeaderSent = effective != config.XFFOff && len(settings.ProxyList) == 0
	s.ProxyMode = len(settings.ProxyList) > 0

	p.mu.Lock()
	inFlight := len(p.running)
	p.mu.Unlock()

	return Stats{Rotator: s, InFlight: inFlight, MaxConcurrent: settings.MaxConcurrent}
}

// Rotator exposes the identity rotator for stats reset.
func (p *Pipeline) Rotator() *identity.Rotator { return p.rot }

// Submit --------------------------------------------------------------------

// Submit validates the image, drives the upstream submit with identity rotation
// and registers the resulting task.
func (p *Pipeline) Submit(ctx context.Context, req SubmitRequest) (*model.Task, error) {
	settings := p.store.Settings()

	if len(req.Image) == 0 {
		return nil, fmt.Errorf("%w: empty image payload", ErrImageRejected)
	}
	if int64(len(req.Image)) > settings.ImageMaxBytes {
		return nil, fmt.Errorf("%w: image is %d bytes, limit is %d", ErrImageRejected, len(req.Image), settings.ImageMaxBytes)
	}
	if !upstream.ValidImage(req.Image) {
		return nil, fmt.Errorf("%w: only JPEG, PNG and WebP are accepted", ErrImageRejected)
	}
	if req.Duration == 0 {
		req.Duration = 6
	}
	if req.Ratio == "" {
		req.Ratio = "9:16"
	}
	if req.Model == "" {
		req.Model = "minimax-h3"
	}

	p.sem.SetMax(settings.MaxConcurrent)
	release, err := p.sem.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	effective := identity.EffectiveMode(settings, p.probeFn())
	family := identity.PickFamily(effective)
	poolPolicy := PoolPolicy(settings)

	deadline := time.Now().Add(settings.SubmitTimeout())
	var lastErr error
	authFails := 0

	for attempt := 1; time.Now().Before(deadline); attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ident := p.rot.Acquire(family, poolPolicy, settings.XFFVariants)
		proxy := p.up.PickProxy()
		res, err := p.up.Submit(ctx, upstream.SubmitOptions{
			Image:    req.Image,
			Filename: req.Filename,
			Prompt:   req.Prompt,
			Ratio:    req.Ratio,
			Duration: req.Duration,
		}, ident, proxy)

		if err == nil {
			switch {
			case res.RemainingKnown && res.Remaining <= 0:
				// Upstream says this address is spent.
				p.rot.Burn(ident)
			case res.RemainingKnown:
				// Upstream reports the authoritative remaining count, so adopt it
				// and return the identity without decrementing a second time —
				// doing both would retire an address that still has a use left
				// and halve the effective capacity.
				ident.UsesLeft = res.Remaining
				p.rot.Report(ident, false)
			default:
				// No remaining field: fall back to local accounting.
				p.rot.Report(ident, true)
			}
			task := p.newTask(req, ident, res, proxy, settings)
			p.store.SaveTask(task)
			if err := p.saveUpload(task.ID, req.Image); err != nil {
				p.log.Warn("cannot cache input image for resubmits", "task", task.ID, "err", err)
			}
			p.startPoll(task)
			p.log.Info("task submitted", "task", task.ID, "upstream", res.TaskID,
				"xff", ident.ForgedIP, "family", ident.Family, "attempt", attempt)
			return task, nil
		}

		var ue *upstream.Error
		if !errors.As(err, &ue) {
			return nil, err
		}
		lastErr = ue

		switch {
		case ue.IsRateLimit():
			// This forged address is spent; discard it and mint a new identity.
			p.rot.Burn(ident)
			p.log.Debug("quota exhausted for identity, rotating",
				"xff", ident.ForgedIP, "attempt", attempt)
			sleepCtx(ctx, jitter(400*time.Millisecond, 800*time.Millisecond))
			continue

		case ue.IsAuthGate():
			// Upstream occasionally gates a single identity. Rotating usually
			// clears it; a run of failures means a policy change, not bad luck.
			p.rot.Burn(ident)
			authFails++
			if authFails >= 6 {
				return nil, fmt.Errorf("upstream refused every fresh identity (%s); the anonymous channel appears gated: %w", ue.Code, ue)
			}
			p.log.Debug("upstream auth gate, rotating", "code", ue.Code, "attempt", attempt)
			sleepCtx(ctx, jitter(400*time.Millisecond, 800*time.Millisecond))
			continue

		case ue.IsRetryable():
			p.rot.Report(ident, false)
			backoff := time.Duration(minInt(2000*attempt, 8000)) * time.Millisecond
			p.log.Debug("transient upstream error, backing off", "err", ue.Error(), "backoff", backoff)
			sleepCtx(ctx, backoff)
			continue

		default:
			// A 4xx means the request itself is wrong; retrying cannot help.
			p.rot.Report(ident, false)
			return nil, ue
		}
	}

	if lastErr == nil {
		lastErr = ErrUpstreamBusy
	}
	return nil, fmt.Errorf("%w within %s: %v", ErrUpstreamBusy, settings.SubmitTimeout(), lastErr)
}

// PoolPolicy resolves the configured address pool.
func PoolPolicy(s config.Settings) string {
	// The documentation ranges are offered for operators who would rather not
	// share quota buckets with real clients behind a forged public address.
	if s.XFFPool == config.XFFPoolReserved {
		return identity.PoolReserved
	}
	return identity.PoolPublic
}

func (p *Pipeline) newTask(req SubmitRequest, ident *identity.Identity, res *upstream.SubmitResult,
	proxy string, settings config.Settings) *model.Task {

	now := time.Now()
	t := &model.Task{
		ID:             "video_" + randHex(16),
		Status:         model.StatusQueued,
		Model:          req.Model,
		Ratio:          req.Ratio,
		Duration:       req.Duration,
		Prompt:         req.Prompt,
		UpstreamTaskID: res.TaskID,
		AccessToken:    res.AccessToken,
		ClientID:       ident.ClientID,
		VisitorID:      ident.VisitorID,
		IPFamily:       ident.Family,
		CreatedAt:      now,
		UpdatedAt:      now,
		APIKeyID:       req.APIKeyID,
		APIKeyTag:      req.APIKeyTag,
		Source:         req.Source,
	}
	if proxy == "" {
		t.ForgedIP = ident.ForgedIP
	}
	if res.Status != "" {
		t.Status = normalizeStatus(res.Status)
	}
	return t
}

// Polling -------------------------------------------------------------------

func (p *Pipeline) startPoll(t *model.Task) {
	p.mu.Lock()
	if _, exists := p.running[t.ID]; exists {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(p.ctx)
	p.running[t.ID] = cancel
	p.mu.Unlock()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() {
			p.mu.Lock()
			delete(p.running, t.ID)
			p.mu.Unlock()
		}()
		p.pollLoop(ctx, t.ID)
	}()
}

// CancelTask stops polling a task.
func (p *Pipeline) CancelTask(taskID string) {
	p.mu.Lock()
	cancel, ok := p.running[taskID]
	p.mu.Unlock()
	if ok {
		cancel()
	}
}

var retryableUpstreamError = regexp.MustCompile(`(?i)could not be started|try again|retryable|overload|timeout|timed out|busy|unavailable`)

func (p *Pipeline) pollLoop(ctx context.Context, taskID string) {
	for {
		settings := p.store.Settings()
		if !sleepCtx(ctx, settings.PollInterval()+jitter(0, time.Second)) {
			return
		}
		t, ok := p.store.GetTask(taskID)
		if !ok || t.Terminal() {
			return
		}

		ident := &identity.Identity{
			ClientID:  t.ClientID,
			VisitorID: t.VisitorID,
			ForgedIP:  t.ForgedIP,
			Family:    t.IPFamily,
		}
		res, err := p.up.Poll(ctx, t.UpstreamTaskID, t.AccessToken, ident, p.up.PickProxy())
		if err != nil {
			var ue *upstream.Error
			if errors.As(err, &ue) {
				if ue.Status == 404 {
					p.finish(t, model.StatusFailed, "upstream 找不到该任务或已过期（trial task expired）")
					return
				}
				if ue.IsRetryable() {
					p.log.Debug("poll failed transiently", "task", taskID, "err", ue.Error())
					continue
				}
				p.finish(t, model.StatusFailed, fmt.Sprintf("%s: %s", ue.Code, ue.Message))
				return
			}
			if ctx.Err() != nil {
				return
			}
			p.log.Debug("poll error", "task", taskID, "err", err)
			continue
		}

		status := normalizeStatus(res.Status)
		t.UpdatedAt = time.Now()

		if status == model.StatusFailed && retryableUpstreamError.MatchString(errorText(res)) &&
			t.Attempts < settings.TaskResubmits {
			// Upstream workers flap in minutes-long windows, so back off before
			// each resubmit instead of burning every attempt at once.
			base := time.Duration(settings.ResubmitBackoffSec) * time.Second
			backoff := base * time.Duration(t.Attempts+1)
			if max := 5 * base; backoff > max {
				backoff = max
			}
			p.log.Info("upstream task died, will resubmit", "task", taskID,
				"attempt", t.Attempts+1, "backoff", backoff)
			if !sleepCtx(ctx, backoff+jitter(0, base/3)) {
				return
			}
			if p.resubmit(ctx, t) {
				continue
			}
			// Fall through and record the failure when resubmit is impossible.
		}

		switch status {
		case model.StatusSucceeded:
			t.Status = model.StatusSucceeded
			p.store.SaveTask(t)
			p.cacheVideo(ctx, t)
			p.cleanupUpload(t.ID)
			return
		case model.StatusFailed, model.StatusCanceled:
			msg := errorText(res)
			if msg == "" {
				msg = "upstream reported failure"
			}
			p.finish(t, model.StatusFailed, msg)
			return
		default:
			t.Status = status
			p.store.SaveTask(t)
		}
	}
}

// resubmit re-posts the cached input image under a fresh identity after upstream
// dropped the task.
func (p *Pipeline) resubmit(ctx context.Context, t *model.Task) bool {
	data, err := os.ReadFile(p.UploadPath(t.ID))
	if err != nil {
		return false
	}
	settings := p.store.Settings()
	effective := identity.EffectiveMode(settings, p.probeFn())
	ident := p.rot.Mint(identity.PickFamily(effective), PoolPolicy(settings), settings.XFFVariants)
	proxy := p.up.PickProxy()

	res, err := p.up.Submit(ctx, upstream.SubmitOptions{
		Image:    data,
		Filename: t.ID + ".jpg",
		Prompt:   t.Prompt,
		Ratio:    t.Ratio,
		Duration: t.Duration,
	}, ident, proxy)
	if err != nil {
		p.log.Warn("resubmit failed", "task", t.ID, "err", err)
		t.Error = "resubmit failed: " + err.Error()
		return false
	}

	t.UpstreamTaskID = res.TaskID
	t.AccessToken = res.AccessToken
	t.ClientID = ident.ClientID
	t.VisitorID = ident.VisitorID
	t.IPFamily = ident.Family
	t.ForgedIP = ""
	if proxy == "" {
		t.ForgedIP = ident.ForgedIP
	}
	t.Attempts++
	t.Status = model.StatusQueued
	t.Error = ""
	t.UpdatedAt = time.Now()
	p.store.SaveTask(t)
	p.log.Info("resubmitted under a fresh identity", "task", t.ID,
		"upstream", res.TaskID, "attempt", t.Attempts, "xff", ident.ForgedIP)
	return true
}

func (p *Pipeline) finish(t *model.Task, status, msg string) {
	t.Status = status
	t.Error = msg
	t.UpdatedAt = time.Now()
	p.store.SaveTask(t)
	p.cleanupUpload(t.ID)
	p.log.Info("task finished", "task", t.ID, "status", status, "error", msg)
}

func (p *Pipeline) saveUpload(taskID string, data []byte) error {
	return os.WriteFile(p.UploadPath(taskID), data, 0o600)
}

func (p *Pipeline) cleanupUpload(taskID string) {
	_ = os.Remove(p.UploadPath(taskID))
}

// cacheVideo best-effort downloads the finished MP4 so it survives upstream's
// 1-2 day trial-task reaping.
func (p *Pipeline) cacheVideo(ctx context.Context, t *model.Task) {
	path := p.VideoPath(t.ID)
	if _, err := os.Stat(path); err == nil {
		return
	}
	if t.UpstreamTaskID == "" {
		return
	}
	ident := &identity.Identity{ClientID: t.ClientID, ForgedIP: t.ForgedIP, Family: t.IPFamily}
	data, _, err := p.up.Content(ctx, t.UpstreamTaskID, t.AccessToken, ident, p.up.PickProxy())
	if err != nil {
		p.log.Warn("cannot cache video, will retry on download", "task", t.ID, "err", err)
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		p.log.Warn("cannot write cached video", "task", t.ID, "err", err)
	}
}

// Content serves the MP4, preferring the local cache and falling back to
// upstream when the cache is cold.
func (p *Pipeline) Content(ctx context.Context, t *model.Task) ([]byte, string, error) {
	path := p.VideoPath(t.ID)
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		return data, "video/mp4", nil
	}
	if t.UpstreamTaskID == "" || t.AccessToken == "" {
		return nil, "", fmt.Errorf("no upstream reference for this task")
	}
	ident := &identity.Identity{ClientID: t.ClientID, ForgedIP: t.ForgedIP, Family: t.IPFamily}
	data, media, err := p.up.Content(ctx, t.UpstreamTaskID, t.AccessToken, ident, p.up.PickProxy())
	if err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		p.log.Warn("cannot cache video after download", "task", t.ID, "err", err)
	}
	return data, media, nil
}

// Retry re-runs a finished task from its cached input image.
func (p *Pipeline) Retry(ctx context.Context, taskID string) (*model.Task, error) {
	t, ok := p.store.GetTask(taskID)
	if !ok {
		return nil, store.ErrNotFound
	}
	data, err := os.ReadFile(p.UploadPath(taskID))
	if err != nil {
		return nil, fmt.Errorf("the input image for this task is no longer cached")
	}
	p.CancelTask(taskID)
	_ = os.Remove(p.VideoPath(taskID))
	return p.Submit(ctx, SubmitRequest{
		Image:     data,
		Filename:  taskID + ".jpg",
		Prompt:    t.Prompt,
		Model:     t.Model,
		Ratio:     t.Ratio,
		Duration:  t.Duration,
		Source:    t.Source,
		APIKeyID:  t.APIKeyID,
		APIKeyTag: t.APIKeyTag,
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func normalizeStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "queued", "pending", "dispatched", "dispatching":
		return model.StatusQueued
	case "running", "processing", "in_progress", "generating":
		return model.StatusRunning
	case "succeeded", "success", "completed", "done", "finished":
		return model.StatusSucceeded
	case "failed", "error", "failure":
		return model.StatusFailed
	case "canceled", "cancelled":
		return model.StatusCanceled
	case "":
		return model.StatusQueued
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

func errorText(res *upstream.PollResult) string {
	for _, s := range []string{res.Error, res.ErrorMsg, res.Message} {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	if len(res.Raw) > 0 {
		var m map[string]any
		if json.Unmarshal(res.Raw, &m) == nil {
			for _, k := range []string{"error", "error_message", "message", "failure_reason"} {
				if v, ok := m[k]; ok {
					if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
						return strings.TrimSpace(s)
					}
				}
			}
		}
		return truncate(string(res.Raw), 500)
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func jitter(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(rand.Int63n(int64(max-min)))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

const hexDigits = "0123456789abcdef"

func randHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = hexDigits[rand.Intn(len(hexDigits))]
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// dynamic semaphore
// ---------------------------------------------------------------------------

type dynSem struct {
	mu  sync.Mutex
	cur int
	max int
}

func newDynSem(max int) *dynSem {
	if max < 1 {
		max = 1
	}
	return &dynSem{max: max}
}

func (s *dynSem) SetMax(n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	s.max = n
	s.mu.Unlock()
}

func (s *dynSem) Acquire(ctx context.Context) (func(), error) {
	for {
		s.mu.Lock()
		if s.cur < s.max {
			s.cur++
			s.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					s.mu.Lock()
					s.cur--
					s.mu.Unlock()
				})
			}, nil
		}
		s.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
