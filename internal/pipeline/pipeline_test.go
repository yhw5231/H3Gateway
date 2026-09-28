package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yhw5231/H3Gateway/internal/config"
	"github.com/yhw5231/H3Gateway/internal/identity"
	"github.com/yhw5231/H3Gateway/internal/model"
	"github.com/yhw5231/H3Gateway/internal/store"
	"github.com/yhw5231/H3Gateway/internal/upstream"
)

// jpegBytes is a minimal payload that sniffs as JPEG, which is all the fake
// upstream validates.
var jpegBytes = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}

// fakeTrial is a scriptable stand-in for the upstream trial channel.
type fakeTrial struct {
	mu sync.Mutex

	// submissions counts accepted generations.
	submissions int
	// statuses is the sequence returned by the poll endpoint. The last entry
	// repeats once exhausted.
	statuses []string
	// pollCount indexes statuses.
	pollCount int
	// rejectIPv6 models an upstream that only honours IPv4 XFF.
	rejectIPv6 bool
	// failFirstSubmit makes the very first generation fail with a retryable
	// error so the resubmit path can be exercised.
	failFirstSubmit bool
	// rateLimited makes every submit answer 429.
	rateLimited bool
	// submitRemaining overrides the "remaining" field of a successful submit.
	// A negative value omits the field entirely, modelling an upstream that does
	// not report quota.
	submitRemaining int
	// omitRemaining drops the "remaining" field from submit answers.
	omitRemaining bool

	videoBody []byte
	// forgedIPs records the XFF value seen on each accepted submit.
	forgedIPs []string
	lastAuth  string
}

func (f *fakeTrial) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/minimax-trial/video-generation", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if f.rateLimited {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"http_code":"429","type":"rate_limit_error","message":"used today"}}`))
			return
		}
		xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
		if f.rejectIPv6 && strings.Contains(xff, ":") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"http_code":"401","type":"login_required","message":"Sign in"}}`))
			return
		}

		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failFirstSubmit && f.submissions == 0 {
			f.submissions++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"http_code":"400","type":"bad_request_error","message":"The video could not be started"}}`))
			return
		}
		f.submissions++
		f.forgedIPs = append(f.forgedIPs, xff)
		id := fmt.Sprintf("up-%d", f.submissions)
		payload := map[string]any{
			"task_id": id, "access_token": "tok-" + id, "status": "queued",
			"limit": 2, "remaining": 1, "max_concurrent": 5,
		}
		if f.omitRemaining {
			delete(payload, "remaining")
		} else if f.submitRemaining < 0 {
			payload["remaining"] = 0
		} else if f.submitRemaining > 0 {
			payload["remaining"] = f.submitRemaining
		}
		writeJSON(w, payload)
	})

	mux.HandleFunc("/api/minimax-trial/video-generation/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/content") {
			if f.lastAuth != "" && r.URL.Query().Get("access_token") == "" {
				http.Error(w, "missing token", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(f.videoBody)
			return
		}
		f.mu.Lock()
		status := "succeeded"
		if len(f.statuses) > 0 {
			idx := f.pollCount
			if idx >= len(f.statuses) {
				idx = len(f.statuses) - 1
			}
			status = f.statuses[idx]
			f.pollCount++
		}
		f.lastAuth = r.URL.Query().Get("access_token")
		f.mu.Unlock()

		payload := map[string]any{"status": status, "task_id": "up"}
		if status == "failed" {
			payload["error"] = "The video could not be started"
		}
		writeJSON(w, payload)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type harness struct {
	pipe    *Pipeline
	store   *store.Store
	fake    *fakeTrial
	srv     *httptest.Server
	videos  string
	uploads string
}

func newHarness(t *testing.T, mutate func(*config.Settings), fake *fakeTrial) *harness {
	t.Helper()
	if fake == nil {
		fake = &fakeTrial{videoBody: []byte("MP4DATA")}
	}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	settings := config.DefaultSettings()
	settings.UpstreamBase = srv.URL
	settings.PollIntervalSec = 0.5
	settings.SubmitTimeoutSec = 30
	settings.ResubmitBackoffSec = 1
	settings.TaskResubmits = 2
	if mutate != nil {
		mutate(&settings)
	}
	settings.Normalize()

	st, err := store.Open(filepath.Join(dir, "h3gateway.json"), settings)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	up := upstream.New(st.Settings)
	t.Cleanup(up.Close)

	videos := filepath.Join(dir, "videos")
	uploads := filepath.Join(dir, "uploads")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipe := New(st, up, identity.NewRotator(), log, videos, uploads, st.Probe)

	ctx, cancel := context.WithCancel(context.Background())
	pipe.Start(ctx)
	t.Cleanup(func() { cancel(); pipe.Stop() })

	return &harness{pipe: pipe, store: st, fake: fake, srv: srv, videos: videos, uploads: uploads}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cond()
}

// ---------------------------------------------------------------------------
// helper units
// ---------------------------------------------------------------------------

func TestNormalizeStatus(t *testing.T) {
	cases := map[string]string{
		"queued": model.StatusQueued, "PENDING": model.StatusQueued, "dispatching": model.StatusQueued,
		"running": model.StatusRunning, "PROCESSING": model.StatusRunning, "generating": model.StatusRunning,
		"succeeded": model.StatusSucceeded, "success": model.StatusSucceeded, "COMPLETED": model.StatusSucceeded,
		"failed": model.StatusFailed, "error": model.StatusFailed,
		"canceled": model.StatusCanceled, "cancelled": model.StatusCanceled,
		"":      model.StatusQueued,
		"weird": "weird",
	}
	for in, want := range cases {
		if got := normalizeStatus(in); got != want {
			t.Fatalf("normalizeStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestErrorTextPrefersStructuredFields(t *testing.T) {
	if got := errorText(&upstream.PollResult{Error: "boom", Message: "other"}); got != "boom" {
		t.Fatalf("errorText = %q", got)
	}
	if got := errorText(&upstream.PollResult{ErrorMsg: "second"}); got != "second" {
		t.Fatalf("errorText = %q", got)
	}
	raw := json.RawMessage(`{"status":"failed","error":"nested"}`)
	if got := errorText(&upstream.PollResult{Raw: raw}); got != "nested" {
		t.Fatalf("errorText = %q", got)
	}
	if got := errorText(&upstream.PollResult{}); got != "" {
		t.Fatalf("errorText = %q, want empty", got)
	}
}

func TestRetryableErrorPattern(t *testing.T) {
	yes := []string{
		"The video could not be started",
		"please try again later",
		"generation_retryable",
		"service overloaded",
		"request timed out",
		"backend busy",
		"temporarily unavailable",
	}
	for _, s := range yes {
		if !retryableUpstreamError.MatchString(s) {
			t.Fatalf("%q should be treated as retryable", s)
		}
	}
	no := []string{"Only JPG, PNG, or WEBP images are supported", "Invalid ratio", "content policy violation"}
	for _, s := range no {
		if retryableUpstreamError.MatchString(s) {
			t.Fatalf("%q should not be treated as retryable", s)
		}
	}
}

func TestDynSemRespectsLimitAndCancellation(t *testing.T) {
	sem := newDynSem(2)
	r1, err := sem.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	r2, _ := sem.Acquire(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if _, err := sem.Acquire(ctx); err == nil {
		t.Fatal("expected Acquire to block until the context expired")
	}

	r1()
	r3, err := sem.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	r2()
	r3()

	// A reduced limit must be honoured immediately.
	sem.SetMax(1)
	ra, _ := sem.Acquire(context.Background())
	ctx2, cancel2 := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel2()
	if _, err := sem.Acquire(ctx2); err == nil {
		t.Fatal("expected the lowered limit to block a second acquire")
	}
	ra()
}

func TestPoolPolicy(t *testing.T) {
	s := config.DefaultSettings()
	if got := PoolPolicy(s); got != identity.PoolPublic {
		t.Fatalf("PoolPolicy = %q", got)
	}
	s.XFFPool = config.XFFPoolReserved
	if got := PoolPolicy(s); got != identity.PoolReserved {
		t.Fatalf("PoolPolicy = %q", got)
	}
}

func TestRandHexIsHexAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		h := randHex(16)
		if len(h) != 16 {
			t.Fatalf("randHex(16) = %q", h)
		}
		for _, c := range h {
			if !strings.ContainsRune(hexDigits, c) {
				t.Fatalf("randHex produced a non-hex character: %q", h)
			}
		}
		if seen[h] {
			t.Fatalf("duplicate id %q", h)
		}
		seen[h] = true
	}
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

func TestSubmitRejectsBadImages(t *testing.T) {
	h := newHarness(t, nil, nil)
	ctx := context.Background()

	if _, err := h.pipe.Submit(ctx, SubmitRequest{}); err == nil {
		t.Fatal("expected an error for an empty payload")
	}
	if _, err := h.pipe.Submit(ctx, SubmitRequest{Image: []byte("not an image")}); err == nil {
		t.Fatal("expected an error for a non-image payload")
	}

	big := make([]byte, 0, 1024)
	big = append(big, jpegBytes...)
	h.store.UpdateSettings(func() config.Settings {
		s := h.store.Settings()
		s.ImageMaxBytes = 64 * 1024
		return s
	}())
	oversized := append(append([]byte{}, jpegBytes...), make([]byte, 100*1024)...)
	if _, err := h.pipe.Submit(ctx, SubmitRequest{Image: oversized}); err == nil {
		t.Fatal("expected an error for an oversized image")
	}
}

// ---------------------------------------------------------------------------
// happy path
// ---------------------------------------------------------------------------

func TestSubmitPollsToSuccessAndCachesVideo(t *testing.T) {
	h := newHarness(t, nil, nil)

	task, err := h.pipe.Submit(context.Background(), SubmitRequest{
		Image: jpegBytes, Filename: "ref.jpg", Prompt: "make it move", Duration: 6,
		Source: "api", APIKeyID: "k1", APIKeyTag: "prod",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if task.ID == "" || !strings.HasPrefix(task.ID, "video_") {
		t.Fatalf("unexpected task id %q", task.ID)
	}
	if task.Status != model.StatusQueued {
		t.Fatalf("status = %q", task.Status)
	}
	if task.ForgedIP == "" {
		t.Fatal("expected a forged XFF value on the task")
	}
	if task.APIKeyTag != "prod" {
		t.Fatalf("api key tag = %q", task.APIKeyTag)
	}

	ok := waitFor(t, 15*time.Second, func() bool {
		cur, found := h.store.GetTask(task.ID)
		return found && cur.Terminal()
	})
	if !ok {
		cur, _ := h.store.GetTask(task.ID)
		t.Fatalf("task never reached a terminal state: %+v", cur)
	}

	got, _ := h.store.GetTask(task.ID)
	if got.Status != model.StatusSucceeded {
		t.Fatalf("status = %q error = %q", got.Status, got.Error)
	}

	// The finished MP4 must be cached locally so it survives upstream reaping.
	videoPath := h.pipe.VideoPath(task.ID)
	if !waitFor(t, 5*time.Second, func() bool { return fileExists(videoPath) }) {
		t.Fatalf("video was not cached at %s", videoPath)
	}
	data, err := os.ReadFile(videoPath)
	if err != nil || string(data) != "MP4DATA" {
		t.Fatalf("cached video = %q err = %v", data, err)
	}

	// Content is served from the cache.
	body, media, err := h.pipe.Content(context.Background(), got)
	if err != nil {
		t.Fatalf("Content: %v", err)
	}
	if string(body) != "MP4DATA" || media != "video/mp4" {
		t.Fatalf("content = %q media = %q", body, media)
	}

	// The input image is dropped once the task succeeds.
	if fileExists(h.pipe.UploadPath(task.ID)) {
		t.Fatal("input image should be cleaned up after success")
	}
}

func TestSuccessfulSubmitKeepsTheSecondUseOfAnAddress(t *testing.T) {
	// Upstream allows two generations per forged address. After the first one
	// succeeds with remaining=1, the identity must go back to the pool instead of
	// being retired, otherwise half of every address is thrown away.
	h := newHarness(t, func(s *config.Settings) { s.XFFMode = config.XFFIPv4 }, nil)

	first, err := h.pipe.Submit(context.Background(), SubmitRequest{Image: jpegBytes, Filename: "r.jpg"})
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if !waitFor(t, 15*time.Second, func() bool {
		cur, ok := h.store.GetTask(first.ID)
		return ok && cur.Terminal()
	}) {
		t.Fatal("first task never settled")
	}

	st := h.pipe.Rotator().Stats()
	if st.IdentitiesExhausted != 0 {
		t.Fatalf("an address with one use left was retired: %+v", st)
	}
	if st.PoolReady != 1 {
		t.Fatalf("expected the identity to be pooled, stats = %+v", st)
	}

	second, err := h.pipe.Submit(context.Background(), SubmitRequest{Image: jpegBytes, Filename: "r.jpg"})
	if err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if second.ForgedIP != first.ForgedIP {
		t.Fatalf("expected the pooled address %q to be reused, got %q", first.ForgedIP, second.ForgedIP)
	}
	if st := h.pipe.Rotator().Stats(); st.IdentitiesReused != 1 {
		t.Fatalf("reuse not recorded: %+v", st)
	}
}

func TestSuccessfulSubmitRetiresAnAddressWithNoQuotaLeft(t *testing.T) {
	// When upstream answers remaining=0 the address must be dropped immediately.
	fake := &fakeTrial{videoBody: []byte("MP4"), submitRemaining: -1}
	h := newHarness(t, func(s *config.Settings) { s.XFFMode = config.XFFIPv4 }, fake)

	if _, err := h.pipe.Submit(context.Background(), SubmitRequest{Image: jpegBytes, Filename: "r.jpg"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	st := h.pipe.Rotator().Stats()
	if st.IdentitiesExhausted != 1 {
		t.Fatalf("expected the spent address to be retired: %+v", st)
	}
	if st.PoolReady != 0 {
		t.Fatalf("a spent address must not be pooled: %+v", st)
	}
}

func TestSuccessfulSubmitWithoutRemainingUsesLocalAccounting(t *testing.T) {
	// Some upstream answers omit "remaining"; the local counter must then be the
	// source of truth so the address is still reused once.
	fake := &fakeTrial{videoBody: []byte("MP4"), omitRemaining: true}
	h := newHarness(t, func(s *config.Settings) { s.XFFMode = config.XFFIPv4 }, fake)

	if _, err := h.pipe.Submit(context.Background(), SubmitRequest{Image: jpegBytes, Filename: "r.jpg"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	st := h.pipe.Rotator().Stats()
	if st.IdentitiesExhausted != 0 || st.PoolReady != 1 {
		t.Fatalf("local accounting should keep one use: %+v", st)
	}
}

func TestSubmitForwardsTheRequestedDuration(t *testing.T) {
	h := newHarness(t, nil, nil)
	if _, err := h.pipe.Submit(context.Background(), SubmitRequest{
		Image: jpegBytes, Filename: "r.jpg", Duration: 15,
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// The fake upstream records nothing about duration, so assert on the stored
	// task, which is what the poller and any resubmit reuse.
	tasks, _ := h.store.ListTasks(store.TaskFilter{Limit: 1})
	if len(tasks) != 1 || tasks[0].Duration != 15 {
		t.Fatalf("task duration = %+v", tasks)
	}
}

// ---------------------------------------------------------------------------
// rotation and failure handling
// ---------------------------------------------------------------------------

func TestSubmitRotatesIdentityOnRateLimit(t *testing.T) {
	fake := &fakeTrial{videoBody: []byte("MP4"), rateLimited: true}
	h := newHarness(t, func(s *config.Settings) { s.SubmitTimeoutSec = 3 }, fake)

	_, err := h.pipe.Submit(context.Background(), SubmitRequest{Image: jpegBytes, Filename: "r.jpg"})
	if err == nil {
		t.Fatal("expected the submit to fail once the deadline passed")
	}
	if !strings.Contains(err.Error(), "upstream submit failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	// Every 429 must burn the identity it used.
	if st := h.pipe.Rotator().Stats(); st.IdentitiesExhausted == 0 {
		t.Fatal("rate-limited identities were not retired")
	}
}

func TestSubmitAvoidsIPv6WhenProbeSaysUnsupported(t *testing.T) {
	fake := &fakeTrial{videoBody: []byte("MP4"), rejectIPv6: true}
	h := newHarness(t, func(s *config.Settings) {
		s.XFFMode = config.XFFIPv6 // deliberately wrong: upstream rejects IPv6
	}, fake)

	// With IPv6 selected but unsupported, every attempt burns an identity until
	// the deadline. The point of the test is that the gateway never silently
	// succeeds with a family the probe did not confirm.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_, err := h.pipe.Submit(ctx, SubmitRequest{Image: jpegBytes, Filename: "r.jpg"})
	if err == nil {
		t.Fatal("expected failure when only IPv6 is allowed but unsupported")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, ip := range fake.forgedIPs {
		if !strings.Contains(ip, ":") {
			t.Fatalf("expected only IPv6 values, saw %q", ip)
		}
	}
}

func TestSubmitUsesIPv6WhenProbeConfirmsIt(t *testing.T) {
	fake := &fakeTrial{videoBody: []byte("MP4")}
	h := newHarness(t, func(s *config.Settings) { s.XFFMode = config.XFFAuto }, fake)
	h.store.SetProbe(&model.XFFProbeRecord{
		RanAt: time.Now(), OK: true, IPv4Accepted: true, IPv6Accepted: true,
	})

	task, err := h.pipe.Submit(context.Background(), SubmitRequest{Image: jpegBytes, Filename: "r.jpg"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if task.IPFamily != model.FamilyIPv6 && task.IPFamily != model.FamilyIPv4 {
		t.Fatalf("unexpected family %q", task.IPFamily)
	}
	if task.IPFamily == model.FamilyIPv6 && !strings.Contains(task.ForgedIP, ":") {
		t.Fatalf("family ipv6 but address %q", task.ForgedIP)
	}
}

func TestTaskResubmittedWhenUpstreamDropsIt(t *testing.T) {
	// First poll reports a retryable failure; the resubmit then succeeds.
	fake := &fakeTrial{videoBody: []byte("MP4"), statuses: []string{"failed", "succeeded"}}
	h := newHarness(t, nil, fake)

	task, err := h.pipe.Submit(context.Background(), SubmitRequest{Image: jpegBytes, Filename: "r.jpg"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ok := waitFor(t, 20*time.Second, func() bool {
		cur, found := h.store.GetTask(task.ID)
		return found && cur.Terminal()
	})
	if !ok {
		cur, _ := h.store.GetTask(task.ID)
		t.Fatalf("task never settled: %+v", cur)
	}
	got, _ := h.store.GetTask(task.ID)
	if got.Attempts == 0 {
		t.Fatalf("expected at least one resubmit, task = %+v", got)
	}
	if got.Status != model.StatusSucceeded {
		t.Fatalf("status = %q error = %q", got.Status, got.Error)
	}
	// The resubmit must have used a fresh identity.
	fake.mu.Lock()
	subs := fake.submissions
	fake.mu.Unlock()
	if subs < 2 {
		t.Fatalf("expected 2 upstream submissions, saw %d", subs)
	}
}

func TestTaskFailsWhenResubmitsAreExhausted(t *testing.T) {
	fake := &fakeTrial{videoBody: []byte("MP4"), statuses: []string{"failed"}}
	h := newHarness(t, func(s *config.Settings) { s.TaskResubmits = 1 }, fake)

	task, err := h.pipe.Submit(context.Background(), SubmitRequest{Image: jpegBytes, Filename: "r.jpg"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	ok := waitFor(t, 20*time.Second, func() bool {
		cur, found := h.store.GetTask(task.ID)
		return found && cur.Terminal()
	})
	if !ok {
		t.Fatal("task never settled")
	}
	got, _ := h.store.GetTask(task.ID)
	if got.Status != model.StatusFailed {
		t.Fatalf("status = %q", got.Status)
	}
	if !strings.Contains(got.Error, "could not be started") {
		t.Fatalf("error = %q", got.Error)
	}
}

func TestRetryRepostsFromCachedInput(t *testing.T) {
	h := newHarness(t, nil, nil)
	task, err := h.pipe.Submit(context.Background(), SubmitRequest{Image: jpegBytes, Filename: "r.jpg"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Recreate the cached input because a successful task cleans it up.
	if err := os.WriteFile(h.pipe.UploadPath(task.ID), jpegBytes, 0o600); err != nil {
		t.Fatalf("seed upload: %v", err)
	}
	retried, err := h.pipe.Retry(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if retried.ID == task.ID {
		t.Fatal("Retry should create a new task")
	}
	if _, err := h.pipe.Retry(context.Background(), "video_missing"); err == nil {
		t.Fatal("expected an error for an unknown task")
	}
}

func TestRetryFailsWithoutCachedInput(t *testing.T) {
	h := newHarness(t, nil, nil)
	task, err := h.pipe.Submit(context.Background(), SubmitRequest{Image: jpegBytes, Filename: "r.jpg"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	_ = os.Remove(h.pipe.UploadPath(task.ID))
	if _, err := h.pipe.Retry(context.Background(), task.ID); err == nil {
		t.Fatal("expected Retry to fail once the input image is gone")
	}
}

func TestStatsReflectSettingsAndProbe(t *testing.T) {
	h := newHarness(t, func(s *config.Settings) { s.MaxConcurrent = 3 }, nil)
	h.store.SetProbe(&model.XFFProbeRecord{OK: true, IPv4Accepted: true, IPv6Accepted: true})
	st := h.pipe.Stats()
	if st.MaxConcurrent != 3 {
		t.Fatalf("max concurrent = %d", st.MaxConcurrent)
	}
	if st.Rotator.EffectiveXFFMode != config.XFFMixed {
		t.Fatalf("effective mode = %q", st.Rotator.EffectiveXFFMode)
	}
	if !st.Rotator.ForgedHeaderSent {
		t.Fatal("expected the forged header to be reported as sent")
	}
}

func TestStatsReportProxyMode(t *testing.T) {
	h := newHarness(t, func(s *config.Settings) {
		s.ProxyList = []string{"http://127.0.0.1:3128"}
	}, nil)
	st := h.pipe.Stats()
	if !st.Rotator.ProxyMode {
		t.Fatal("proxy mode not reported")
	}
	if st.Rotator.ForgedHeaderSent {
		t.Fatal("forging must be off when a proxy supplies the real address")
	}
}

func TestStartResumesPendingTasks(t *testing.T) {
	fake := &fakeTrial{videoBody: []byte("MP4")}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	dir := t.TempDir()
	settings := config.DefaultSettings()
	settings.UpstreamBase = srv.URL
	settings.PollIntervalSec = 0.5
	settings.Normalize()

	st, err := store.Open(filepath.Join(dir, "h3gateway.json"), settings)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	// A task left in flight by a previous process.
	now := time.Now()
	st.SaveTask(&model.Task{
		ID: "video_resume", Status: model.StatusRunning, Model: "minimax-h3",
		Ratio: "9:16", Duration: 6, UpstreamTaskID: "up-1", AccessToken: "tok",
		ClientID: "cid", CreatedAt: now, UpdatedAt: now,
	})

	up := upstream.New(st.Settings)
	defer up.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipe := New(st, up, identity.NewRotator(), log,
		filepath.Join(dir, "videos"), filepath.Join(dir, "uploads"), st.Probe)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pipe.Start(ctx)
	defer pipe.Stop()

	if !waitFor(t, 15*time.Second, func() bool {
		cur, ok := st.GetTask("video_resume")
		return ok && cur.Terminal()
	}) {
		cur, _ := st.GetTask("video_resume")
		t.Fatalf("resumed task never settled: %+v", cur)
	}
	got, _ := st.GetTask("video_resume")
	if got.Status != model.StatusSucceeded {
		t.Fatalf("status = %q error = %q", got.Status, got.Error)
	}
}

func TestConcurrentSubmitsRespectTheLimit(t *testing.T) {
	var inFlight, peak int32
	fake := &fakeTrial{videoBody: []byte("MP4")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
				break
			}
		}
		time.Sleep(120 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		fake.handler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	settings := config.DefaultSettings()
	settings.UpstreamBase = srv.URL
	settings.MaxConcurrent = 2
	settings.PollIntervalSec = 0.5
	settings.Normalize()
	st, err := store.Open(filepath.Join(dir, "h3gateway.json"), settings)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	up := upstream.New(st.Settings)
	defer up.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipe := New(st, up, identity.NewRotator(), log,
		filepath.Join(dir, "videos"), filepath.Join(dir, "uploads"), st.Probe)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = pipe.Submit(context.Background(), SubmitRequest{Image: jpegBytes, Filename: "r.jpg"})
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&peak); got > 2 {
		t.Fatalf("concurrency limit exceeded: peak %d", got)
	}
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
