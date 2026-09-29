package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/yhw5231/H3Gateway/internal/auth"
	"github.com/yhw5231/H3Gateway/internal/config"
	"github.com/yhw5231/H3Gateway/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "h3gateway.json")
	st, err := Open(path, config.DefaultSettings())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func mkTask(id, status string) *model.Task {
	now := time.Now()
	return &model.Task{ID: id, Status: status, Model: "minimax-h3", Ratio: "9:16",
		Duration: 6, CreatedAt: now, UpdatedAt: now}
}

func TestOpenCreatesFileAndStableSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h3gateway.json")

	st, err := Open(path, config.DefaultSettings())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	secret := st.Secret()
	if secret == "" {
		t.Fatal("store generated an empty secret")
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening must keep the same signing secret so sessions survive restarts.
	st2, err := Open(path, config.DefaultSettings())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	if st2.Secret() != secret {
		t.Fatal("secret changed across restarts")
	}
}

func TestTaskRoundTripAndOrdering(t *testing.T) {
	st := newTestStore(t)
	older := mkTask("video_a", model.StatusSucceeded)
	older.CreatedAt = time.Now().Add(-time.Hour)
	newer := mkTask("video_b", model.StatusQueued)
	st.SaveTask(older)
	st.SaveTask(newer)

	got, ok := st.GetTask("video_a")
	if !ok || got.Status != model.StatusSucceeded {
		t.Fatalf("GetTask returned %+v ok=%v", got, ok)
	}

	list, total := st.ListTasks(TaskFilter{})
	if total != 2 || len(list) != 2 {
		t.Fatalf("ListTasks = %d items, total %d", len(list), total)
	}
	if list[0].ID != "video_b" {
		t.Fatalf("expected newest first, got %s", list[0].ID)
	}
}

func TestListTasksFilters(t *testing.T) {
	st := newTestStore(t)
	st.SaveTask(mkTask("video_ok", model.StatusSucceeded))
	st.SaveTask(mkTask("video_bad", model.StatusFailed))
	slow := mkTask("video_slow", model.StatusRunning)
	slow.ForgedIP = "2001:db8::abc"
	st.SaveTask(slow)

	if list, total := st.ListTasks(TaskFilter{Status: model.StatusFailed}); total != 1 || list[0].ID != "video_bad" {
		t.Fatalf("status filter = %v total %d", list, total)
	}
	if _, total := st.ListTasks(TaskFilter{Search: "2001:db8"}); total != 1 {
		t.Fatalf("search by forged ip total = %d", total)
	}
	if _, total := st.ListTasks(TaskFilter{Search: "VIDEO_OK"}); total != 1 {
		t.Fatalf("search should be case-insensitive, total = %d", total)
	}
	list, total := st.ListTasks(TaskFilter{Limit: 2, Offset: 2})
	if total != 3 || len(list) != 1 {
		t.Fatalf("pagination = %d items, total %d", len(list), total)
	}
}

func TestPendingTasksOnlyReturnsResumableWork(t *testing.T) {
	st := newTestStore(t)
	resumable := mkTask("video_r", model.StatusRunning)
	resumable.UpstreamTaskID = "up-1"
	resumable.AccessToken = "tok"
	st.SaveTask(resumable)

	st.SaveTask(mkTask("video_done", model.StatusSucceeded))

	noToken := mkTask("video_nt", model.StatusQueued)
	noToken.UpstreamTaskID = "up-2"
	st.SaveTask(noToken)

	pending := st.PendingTasks()
	if len(pending) != 1 || pending[0].ID != "video_r" {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestTaskByUpstreamID(t *testing.T) {
	st := newTestStore(t)
	first := mkTask("video_a", model.StatusSucceeded)
	first.UpstreamTaskID = "up-1"
	st.SaveTask(first)

	second := mkTask("video_b", model.StatusQueued)
	second.UpstreamTaskID = "up-2"
	st.SaveTask(second)

	got, ok := st.TaskByUpstreamID("up-2")
	if !ok || got.ID != "video_b" {
		t.Fatalf("TaskByUpstreamID(up-2) = %+v, %v", got, ok)
	}
	if _, ok := st.TaskByUpstreamID("up-unknown"); ok {
		t.Fatal("an unknown upstream id must not match")
	}
	if _, ok := st.TaskByUpstreamID(""); ok {
		t.Fatal("an empty upstream id must not match")
	}
}

func TestDeleteTask(t *testing.T) {
	st := newTestStore(t)
	st.SaveTask(mkTask("video_x", model.StatusQueued))
	if _, ok := st.DeleteTask("video_x"); !ok {
		t.Fatal("DeleteTask reported a miss for an existing task")
	}
	if _, ok := st.GetTask("video_x"); ok {
		t.Fatal("task survived deletion")
	}
	if _, ok := st.DeleteTask("video_x"); ok {
		t.Fatal("second delete should report a miss")
	}
}

func TestRetentionPrunesOldTerminalTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h3gateway.json")
	settings := config.DefaultSettings()
	settings.TaskRetention = 50 // minimum accepted by Normalize
	st, err := Open(path, settings)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	var removed []string
	st.OnTaskRemoved = func(t *model.Task) { removed = append(removed, t.ID) }

	base := time.Now()
	for i := 0; i < 60; i++ {
		task := mkTask("video_"+string(rune('a'+i%26))+string(rune('0'+i/26)), model.StatusSucceeded)
		task.CreatedAt = base.Add(time.Duration(i) * time.Second)
		st.SaveTask(task)
	}
	if got := len(st.Stats().ByStatus); got == 0 {
		t.Fatal("expected some tasks to remain")
	}
	if len(removed) == 0 {
		t.Fatal("retention pruning never reported a removed task")
	}
	all, total := st.ListTasks(TaskFilter{Limit: 1000})
	if total > 50 {
		t.Fatalf("retention kept %d tasks, limit is 50", total)
	}
	_ = all
}

func TestRetentionKeepsInFlightTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h3gateway.json")
	settings := config.DefaultSettings()
	settings.TaskRetention = 50
	st, err := Open(path, settings)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	base := time.Now()
	// One ancient in-flight task that must survive, then enough finished work to
	// push past the retention limit.
	live := mkTask("video_live", model.StatusRunning)
	live.CreatedAt = base.Add(-100 * time.Hour)
	st.SaveTask(live)

	for i := 0; i < 80; i++ {
		task := mkTask("video_f"+string(rune('a'+i%26))+string(rune('0'+i/26)), model.StatusSucceeded)
		task.CreatedAt = base.Add(time.Duration(i) * time.Second)
		st.SaveTask(task)
	}
	if _, ok := st.GetTask("video_live"); !ok {
		t.Fatal("retention evicted an in-flight task")
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	st := newTestStore(t)
	if st.HasEnabledKeys() {
		t.Fatal("a fresh store should have no enabled keys")
	}
	value, _ := auth.GenerateAPIKey()
	now := time.Now()
	st.AddKey(&model.APIKey{ID: "k1", Name: "prod", Key: value, Enabled: true, CreatedAt: now, UpdatedAt: now})

	if !st.HasEnabledKeys() {
		t.Fatal("HasEnabledKeys should be true after adding an enabled key")
	}
	if k := st.FindKey(value); k == nil || k.ID != "k1" {
		t.Fatalf("FindKey returned %+v", k)
	}
	if k := st.FindKey("h3-not-a-real-key"); k != nil {
		t.Fatal("FindKey accepted an unknown token")
	}

	st.TouchKey("k1")
	if k, _ := st.GetKey("k1"); k.Requests != 1 || k.LastUsedAt.IsZero() {
		t.Fatalf("TouchKey did not record usage: %+v", k)
	}

	if _, ok := st.UpdateKey("k1", func(k *model.APIKey) { k.Enabled = false }); !ok {
		t.Fatal("UpdateKey missed")
	}
	if k := st.FindKey(value); k != nil {
		t.Fatal("a disabled key must not authenticate")
	}
	if st.HasEnabledKeys() {
		t.Fatal("HasEnabledKeys should be false once every key is disabled")
	}

	if _, ok := st.UpdateKey("nope", func(k *model.APIKey) {}); ok {
		t.Fatal("UpdateKey should miss for an unknown id")
	}
	if !st.DeleteKey("k1") {
		t.Fatal("DeleteKey missed")
	}
	if st.DeleteKey("k1") {
		t.Fatal("second DeleteKey should miss")
	}
}

func TestExpiredKeyIsRejected(t *testing.T) {
	st := newTestStore(t)
	value, _ := auth.GenerateAPIKey()
	past := time.Now().Add(-time.Hour)
	now := time.Now()
	st.AddKey(&model.APIKey{ID: "k1", Name: "old", Key: value, Enabled: true,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: &past})

	if st.FindKey(value) != nil {
		t.Fatal("an expired key must not authenticate")
	}
	if st.HasEnabledKeys() {
		t.Fatal("an expired key must not count as enabled")
	}
}

func TestDeleteUser(t *testing.T) {
	st := newTestStore(t)
	st.UpsertUser(&model.AdminUser{Username: "admin", PasswordHash: "h", Salt: "s", Iterations: 1000})
	if _, ok := st.GetUser("admin"); !ok {
		t.Fatal("user was not stored")
	}
	if !st.DeleteUser("admin") {
		t.Fatal("DeleteUser missed an existing account")
	}
	if _, ok := st.GetUser("admin"); ok {
		t.Fatal("account survived deletion")
	}
	if st.DeleteUser("admin") {
		t.Fatal("second DeleteUser should report a miss")
	}
	if len(st.Users()) != 0 {
		t.Fatalf("Users() = %v", st.Users())
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	st := newTestStore(t)
	next := st.Settings()
	next.XFFMode = config.XFFIPv6
	next.MaxConcurrent = 9
	st.UpdateSettings(next)

	got := st.Settings()
	if got.XFFMode != config.XFFIPv6 || got.MaxConcurrent != 9 {
		t.Fatalf("settings did not persist: %+v", got)
	}
	// Mutating the returned copy must not affect the store.
	got.MaxConcurrent = 1
	if st.Settings().MaxConcurrent != 9 {
		t.Fatal("Settings() leaked a mutable reference")
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h3gateway.json")

	st, err := Open(path, config.DefaultSettings())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	task := mkTask("video_persist", model.StatusSucceeded)
	task.ForgedIP = "2001:db8::1"
	task.IPFamily = model.FamilyIPv6
	st.SaveTask(task)
	value, _ := auth.GenerateAPIKey()
	now := time.Now()
	st.AddKey(&model.APIKey{ID: "k1", Name: "keep", Key: value, Enabled: true, CreatedAt: now, UpdatedAt: now})
	st.UpsertUser(&model.AdminUser{Username: "admin", PasswordHash: "h", Salt: "s", Iterations: 1000})
	st.SetProbe(&model.XFFProbeRecord{RanAt: now, OK: true, IPv6Accepted: true, Message: "ok"})
	settings := st.Settings()
	settings.XFFMode = config.XFFMixed
	st.UpdateSettings(settings)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st2, err := Open(path, config.DefaultSettings())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()

	got, ok := st2.GetTask("video_persist")
	if !ok || got.ForgedIP != "2001:db8::1" || got.IPFamily != model.FamilyIPv6 {
		t.Fatalf("task did not survive reopen: %+v", got)
	}
	if k := st2.FindKey(value); k == nil || k.Name != "keep" {
		t.Fatalf("api key did not survive reopen: %+v", k)
	}
	if _, ok := st2.GetUser("admin"); !ok {
		t.Fatal("admin user did not survive reopen")
	}
	if p := st2.Probe(); p == nil || !p.IPv6Accepted {
		t.Fatalf("probe record did not survive reopen: %+v", p)
	}
	if st2.Settings().XFFMode != config.XFFMixed {
		t.Fatalf("settings did not survive reopen: %+v", st2.Settings())
	}
}

func TestStats(t *testing.T) {
	st := newTestStore(t)
	st.SaveTask(mkTask("video_1", model.StatusSucceeded))
	st.SaveTask(mkTask("video_2", model.StatusFailed))
	value, _ := auth.GenerateAPIKey()
	now := time.Now()
	st.AddKey(&model.APIKey{ID: "k1", Key: value, Enabled: true, CreatedAt: now, UpdatedAt: now})

	s := st.Stats()
	if s.Total != 2 || s.ByStatus[model.StatusSucceeded] != 1 || s.ByStatus[model.StatusFailed] != 1 {
		t.Fatalf("stats = %+v", s)
	}
	if s.Keys != 1 || s.ActiveKeys != 1 || s.TasksToday != 2 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestOverrideSecret(t *testing.T) {
	st := newTestStore(t)
	st.OverrideSecret("shared-secret")
	if st.Secret() != "shared-secret" {
		t.Fatalf("secret = %q", st.Secret())
	}
	st.OverrideSecret("   ")
	if st.Secret() != "shared-secret" {
		t.Fatal("a blank override must be ignored")
	}
}
