// Package store persists tasks, API keys, admin accounts, settings and the
// cached capability probe in a single JSON document.
//
// A JSON snapshot keeps the gateway free of cgo and third-party database drivers
// so the container image can be built from the standard library alone. Writes are
// debounced and atomic (write-to-temp then rename), which is more than enough for
// the task volumes this gateway handles.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yhw5231/H3Gateway/internal/auth"
	"github.com/yhw5231/H3Gateway/internal/config"
	"github.com/yhw5231/H3Gateway/internal/model"
)

// ErrNotFound is returned by lookups that miss.
var ErrNotFound = errors.New("not found")

// documentVersion is bumped when the on-disk layout changes.
const documentVersion = 1

type document struct {
	Version  int                   `json:"version"`
	Secret   string                `json:"secret"`
	Settings *config.Settings      `json:"settings,omitempty"`
	Keys     []*model.APIKey       `json:"keys,omitempty"`
	Users    []*model.AdminUser    `json:"users,omitempty"`
	Tasks    []*model.Task         `json:"tasks,omitempty"`
	Probe    *model.XFFProbeRecord `json:"probe,omitempty"`
}

// Store is a concurrency-safe, file-backed repository.
type Store struct {
	path string

	mu       sync.RWMutex
	secret   string
	settings config.Settings
	keys     []*model.APIKey
	users    map[string]*model.AdminUser
	tasks    map[string]*model.Task
	order    []string // task ids, newest first
	probe    *model.XFFProbeRecord

	// OnTaskRemoved, when set, is invoked for every task dropped by retention
	// pruning so the caller can delete cached media.
	OnTaskRemoved func(*model.Task)

	dirty     bool
	flushErr  error
	stopCh    chan struct{}
	stoppedCh chan struct{}
	closeOnce sync.Once
}

// Open loads (or creates) the store at path. defaults seed the settings before
// any persisted override is applied.
func Open(path string, defaults config.Settings) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	s := &Store{
		path:      path,
		settings:  defaults.Clone(),
		users:     map[string]*model.AdminUser{},
		tasks:     map[string]*model.Task{},
		stopCh:    make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		var doc document
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		s.secret = doc.Secret
		if doc.Settings != nil {
			merged := *doc.Settings
			merged.Normalize()
			s.settings = merged
		}
		s.keys = doc.Keys
		for _, u := range doc.Users {
			s.users[u.Username] = u
		}
		for _, t := range doc.Tasks {
			if t == nil || t.ID == "" {
				continue
			}
			s.tasks[t.ID] = t
		}
		s.probe = doc.Probe
		s.rebuildOrder()
	case os.IsNotExist(err):
		// first run
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	if s.secret == "" {
		s.secret = auth.RandomSecret()
		s.dirty = true
	}
	s.settings.Normalize()
	s.pruneLocked()

	go s.flusher()
	return s, nil
}

// Path reports the backing file location.
func (s *Store) Path() string { return s.path }

// Secret returns the stable HMAC key used to sign sessions and studio cookies.
func (s *Store) Secret() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.secret
}

// OverrideSecret replaces the signing key, used when GATEWAY_SESSION_SECRET is
// supplied so several replicas can share sessions.
func (s *Store) OverrideSecret(secret string) {
	if strings.TrimSpace(secret) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secret != secret {
		s.secret = secret
		s.dirty = true
	}
}

// ---------------------------------------------------------------------------
// tasks
// ---------------------------------------------------------------------------

// SaveTask inserts or replaces a task and schedules a flush.
func (s *Store) SaveTask(t *model.Task) {
	if t == nil || t.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tasks[t.ID]; !exists {
		s.order = append([]string{t.ID}, s.order...)
	}
	s.tasks[t.ID] = t
	s.pruneLocked()
	s.dirty = true
}

// GetTask looks a task up by id.
func (s *Store) GetTask(id string) (*model.Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[id]
	return t, ok
}

// TaskByUpstreamID returns the task already tracking an upstream task id.
//
// The gateway uses this to notice that upstream answered a fresh submission
// with a task it had created earlier (an idempotent replay, or a spent quota key
// handing back its last task). Recording such an answer as a new task would make
// the caller download a video that was already delivered.
func (s *Store) TaskByUpstreamID(upstreamID string) (*model.Task, bool) {
	if upstreamID == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, id := range s.order {
		t := s.tasks[id]
		if t != nil && t.UpstreamTaskID == upstreamID {
			return t, true
		}
	}
	return nil, false
}

// TaskByVideoHash returns the oldest task that already delivered these exact
// video bytes, i.e. whoever produced that content first.
//
// A task id only proves which job upstream accepted, not what it rendered: an
// upstream that hands a canned render to a fresh task id would look healthy
// while every caller downloads the same film. Comparing content hashes is the
// only check that catches it.
func (s *Store) TaskByVideoHash(hash string) (*model.Task, bool) {
	if hash == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := len(s.order) - 1; i >= 0; i-- {
		if t := s.tasks[s.order[i]]; t != nil && t.VideoSHA256 == hash {
			return t, true
		}
	}
	return nil, false
}

// TaskFilter narrows ListTasks.
type TaskFilter struct {
	Status string
	Search string
	Limit  int
	Offset int
}

// ListTasks returns tasks newest-first plus the total number matching the filter
// (before pagination).
func (s *Store) ListTasks(f TaskFilter) ([]*model.Task, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	search := strings.ToLower(strings.TrimSpace(f.Search))
	matched := make([]*model.Task, 0, len(s.order))
	for _, id := range s.order {
		t := s.tasks[id]
		if t == nil {
			continue
		}
		if f.Status != "" && f.Status != "all" && t.Status != f.Status {
			continue
		}
		if search != "" && !taskMatches(t, search) {
			continue
		}
		matched = append(matched, t)
	}
	total := len(matched)
	if f.Offset > 0 {
		if f.Offset >= len(matched) {
			return nil, total
		}
		matched = matched[f.Offset:]
	}
	if f.Limit > 0 && len(matched) > f.Limit {
		matched = matched[:f.Limit]
	}
	return matched, total
}

func taskMatches(t *model.Task, needle string) bool {
	return strings.Contains(strings.ToLower(t.ID), needle) ||
		strings.Contains(strings.ToLower(t.Status), needle) ||
		strings.Contains(strings.ToLower(t.Model), needle) ||
		strings.Contains(strings.ToLower(t.ForgedIP), needle) ||
		strings.Contains(strings.ToLower(t.APIKeyTag), needle) ||
		strings.Contains(strings.ToLower(t.UpstreamTaskID), needle) ||
		strings.Contains(strings.ToLower(t.Prompt), needle)
}

// DeleteTask removes a task and reports whether it existed.
func (s *Store) DeleteTask(id string) (*model.Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, false
	}
	delete(s.tasks, id)
	for i, oid := range s.order {
		if oid == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.dirty = true
	return t, true
}

// PendingTasks returns tasks that still need polling after a restart.
func (s *Store) PendingTasks() []*model.Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.Task
	for _, id := range s.order {
		t := s.tasks[id]
		if t == nil {
			continue
		}
		if (t.Status == model.StatusQueued || t.Status == model.StatusRunning) &&
			t.UpstreamTaskID != "" && t.AccessToken != "" {
			out = append(out, t)
		}
	}
	return out
}

// Stats aggregates counters for the dashboard.
type Stats struct {
	Total      int            `json:"total"`
	ByStatus   map[string]int `json:"by_status"`
	Keys       int            `json:"api_keys"`
	ActiveKeys int            `json:"active_api_keys"`
	TasksToday int            `json:"tasks_today"`
}

// Stats computes dashboard counters.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := Stats{Total: len(s.tasks), ByStatus: map[string]int{}}
	dayAgo := time.Now().Add(-24 * time.Hour)
	for _, t := range s.tasks {
		out.ByStatus[t.Status]++
		if t.CreatedAt.After(dayAgo) {
			out.TasksToday++
		}
	}
	out.Keys = len(s.keys)
	for _, k := range s.keys {
		if k.Enabled {
			out.ActiveKeys++
		}
	}
	return out
}

// pruneLocked trims history down to the configured retention, dropping the
// oldest terminal tasks first. Callers must hold the write lock.
func (s *Store) pruneLocked() {
	limit := s.settings.TaskRetention
	if limit <= 0 || len(s.order) <= limit {
		return
	}
	keep := make([]string, 0, limit)
	var dropped []*model.Task
	for _, id := range s.order {
		t := s.tasks[id]
		if t == nil {
			continue
		}
		if len(keep) < limit {
			keep = append(keep, id)
			continue
		}
		// Only terminal tasks are eligible for eviction; in-flight work is kept
		// so the poller cannot lose track of it.
		if t.Terminal() {
			delete(s.tasks, id)
			if s.OnTaskRemoved != nil {
				dropped = append(dropped, t)
			}
			continue
		}
		keep = append(keep, id)
	}
	s.order = keep
	s.dirty = true
	for _, t := range dropped {
		s.OnTaskRemoved(t)
	}
}

func (s *Store) rebuildOrder() {
	s.order = s.order[:0]
	for id, t := range s.tasks {
		_ = t
		s.order = append(s.order, id)
	}
	sort.Slice(s.order, func(i, j int) bool {
		a, b := s.tasks[s.order[i]], s.tasks[s.order[j]]
		if a == nil || b == nil {
			return s.order[i] < s.order[j]
		}
		return a.CreatedAt.After(b.CreatedAt)
	})
}

// ---------------------------------------------------------------------------
// api keys
// ---------------------------------------------------------------------------

// ListKeys returns a copy of the key list, newest first.
func (s *Store) ListKeys() []*model.APIKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.APIKey, len(s.keys))
	copy(out, s.keys)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// FindKey resolves a bearer token. It returns nil when the token is unknown or
// disabled.
func (s *Store) FindKey(token string) *model.APIKey {
	if token == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.keys {
		if auth.EqualSecret(k.Key, token) {
			if !k.Enabled || k.Expired(time.Now()) {
				return nil
			}
			return k
		}
	}
	return nil
}

// GetKey looks a key up by id.
func (s *Store) GetKey(id string) (*model.APIKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.keys {
		if k.ID == id {
			return k, true
		}
	}
	return nil, false
}

// AddKey stores a new key.
func (s *Store) AddKey(k *model.APIKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, k)
	s.dirty = true
}

// UpdateKey mutates a stored key through fn.
func (s *Store) UpdateKey(id string, fn func(*model.APIKey)) (*model.APIKey, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if k.ID == id {
			fn(k)
			k.UpdatedAt = time.Now()
			s.dirty = true
			return k, true
		}
	}
	return nil, false
}

// DeleteKey removes a key.
func (s *Store) DeleteKey(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.keys {
		if k.ID == id {
			s.keys = append(s.keys[:i], s.keys[i+1:]...)
			s.dirty = true
			return true
		}
	}
	return false
}

// TouchKey records usage of a key.
func (s *Store) TouchKey(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if k.ID == id {
			k.Requests++
			k.LastUsedAt = time.Now()
			s.dirty = true
			return
		}
	}
}

// HasEnabledKeys reports whether any usable key exists. When none does and
// require_api_key is off, /v1/* stays open the way the Python gateway behaved.
func (s *Store) HasEnabledKeys() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	for _, k := range s.keys {
		if k.Enabled && !k.Expired(now) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// admin users
// ---------------------------------------------------------------------------

// GetUser looks an admin account up.
func (s *Store) GetUser(name string) (*model.AdminUser, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[name]
	return u, ok
}

// Users lists admin accounts.
func (s *Store) Users() []*model.AdminUser {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.AdminUser, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// UpsertUser stores an admin account.
func (s *Store) UpsertUser(u *model.AdminUser) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[u.Username] = u
	s.dirty = true
}

// DeleteUser removes an admin account. Because sessions are stateless signed
// tokens, deleting the account is what revokes them.
func (s *Store) DeleteUser(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[name]; !ok {
		return false
	}
	delete(s.users, name)
	s.dirty = true
	return true
}

// ---------------------------------------------------------------------------
// settings, secret and probe
// ---------------------------------------------------------------------------

// Settings returns a copy of the live settings.
func (s *Store) Settings() config.Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings.Clone()
}

// UpdateSettings replaces the settings document.
func (s *Store) UpdateSettings(next config.Settings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next.Normalize()
	s.settings = next
	s.dirty = true
}

// Probe returns the cached capability-probe record.
func (s *Store) Probe() *model.XFFProbeRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.probe == nil {
		return nil
	}
	cp := *s.probe
	cp.Steps = append([]string(nil), s.probe.Steps...)
	return &cp
}

// SetProbe stores the capability-probe record.
func (s *Store) SetProbe(r *model.XFFProbeRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probe = r
	s.dirty = true
}

// ---------------------------------------------------------------------------
// persistence
// ---------------------------------------------------------------------------

func (s *Store) flusher() {
	defer close(s.stoppedCh)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			_ = s.Flush()
			return
		case <-t.C:
			_ = s.Flush()
		}
	}
}

// Flush writes the document if anything changed.
func (s *Store) Flush() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return s.flushErr
	}
	doc := document{
		Version:  documentVersion,
		Secret:   s.secret,
		Settings: &s.settings,
		Keys:     s.keys,
		Users:    make([]*model.AdminUser, 0, len(s.users)),
		Tasks:    make([]*model.Task, 0, len(s.order)),
		Probe:    s.probe,
	}
	for _, u := range s.users {
		doc.Users = append(doc.Users, u)
	}
	sort.Slice(doc.Users, func(i, j int) bool { return doc.Users[i].Username < doc.Users[j].Username })
	for _, id := range s.order {
		if t := s.tasks[id]; t != nil {
			doc.Tasks = append(doc.Tasks, t)
		}
	}
	s.dirty = false
	s.mu.Unlock()

	payload, err := json.MarshalIndent(&doc, "", "  ")
	if err != nil {
		s.setFlushErr(err)
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		s.setFlushErr(err)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		s.setFlushErr(err)
		return err
	}
	s.setFlushErr(nil)
	return nil
}

func (s *Store) setFlushErr(err error) {
	s.mu.Lock()
	s.flushErr = err
	if err != nil {
		s.dirty = true
	}
	s.mu.Unlock()
}

// Close stops the background flusher and writes a final snapshot.
func (s *Store) Close() error {
	s.closeOnce.Do(func() { close(s.stopCh) })
	<-s.stoppedCh
	return s.Flush()
}
