package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// trackingRegistrar records reconciliation calls and can inject a failure.
type trackingRegistrar struct {
	reconcileCalls []string
	reconcileErr   error
}

func (r *trackingRegistrar) Register(context.Context, MonitoredMedia) error { return nil }

func (r *trackingRegistrar) Reconcile(_ context.Context, source string, _ []string, _ []int) error {
	r.reconcileCalls = append(r.reconcileCalls, source)
	return r.reconcileErr
}

func newPassTestMonitor(t *testing.T) *mediaMonitor {
	t.Helper()
	m := newMediaMonitor(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	dir := t.TempDir()
	m.config.File = filepath.Join(dir, "queue.json")
	m.config.ProwlarrIndexFile = filepath.Join(dir, "index.json")
	return m
}

func movieQueueItem(key string) monitoredMedia {
	return monitoredMedia{
		Key:       key,
		MediaType: "movie",
		SourceKey: "request:" + key,
		Title:     key,
	}
}

func readMonitorCursor(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	var state monitorState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode queue file: %v", err)
	}
	return state.Cursor
}

// TestRunPassResumesAfterBudgetExhaustion proves that a pass cut off by its
// deadline reports success, persists the position of the last completed item,
// and that the next pass continues from there instead of restarting.
func TestRunPassResumesAfterBudgetExhaustion(t *testing.T) {
	m := newPassTestMonitor(t)
	// Make the per-item bound irrelevant: the test exercises the pass deadline.
	m.itemTimeout = time.Hour

	items := []monitoredMedia{
		movieQueueItem("a"),
		movieQueueItem("b"),
		movieQueueItem("c"),
		movieQueueItem("d"),
		movieQueueItem("e"),
	}

	blocked := true
	var attemptedFirst, attemptedSecond []string
	m.evaluateFn = func(ctx context.Context, item monitoredMedia) (monitoredMedia, string, error) {
		if item.Key == "c" && blocked {
			<-ctx.Done()
			return item, "", ctx.Err()
		}
		return item, "", nil
	}

	// Wrap to record attempts per pass.
	inner := m.evaluateFn
	m.evaluateFn = func(ctx context.Context, item monitoredMedia) (monitoredMedia, string, error) {
		attemptedFirst = append(attemptedFirst, item.Key)
		return inner(ctx, item)
	}

	firstRegistrar := &trackingRegistrar{}
	passCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	resp, err := m.runPass(passCtx, items, firstRegistrar)
	cancel()
	if err != nil {
		t.Fatalf("budget-exhausted pass returned error: %v", err)
	}
	if len(firstRegistrar.reconcileCalls) != 0 {
		t.Fatalf("budget-exhausted pass reconciled %v; a partial keep set must not sweep", firstRegistrar.reconcileCalls)
	}
	if got := resp.Output["budget_exhausted"]; got != true {
		t.Fatalf("budget_exhausted = %v, want true", got)
	}
	if got := resp.Output["processed"]; got != 2 {
		t.Fatalf("processed = %v, want 2 (a,b before the deadline)", got)
	}
	if cursor := m.currentCursor(); cursor != "b" {
		t.Fatalf("cursor = %q, want %q", cursor, "b")
	}
	if cursor := readMonitorCursor(t, m.config.File); cursor != "b" {
		t.Fatalf("persisted cursor = %q, want %q", cursor, "b")
	}
	for _, key := range attemptedFirst {
		if key == "d" || key == "e" {
			t.Fatalf("item %q attempted in the budget-exhausted pass", key)
		}
	}

	// Second pass: the poison condition is gone, so the pass completes.
	blocked = false
	m.evaluateFn = func(ctx context.Context, item monitoredMedia) (monitoredMedia, string, error) {
		attemptedSecond = append(attemptedSecond, item.Key)
		return inner(ctx, item)
	}
	resp, err = m.runPass(context.Background(), items, &trackingRegistrar{})
	if err != nil {
		t.Fatalf("resumed pass returned error: %v", err)
	}
	if got := resp.Output["budget_exhausted"]; got != false {
		t.Fatalf("resumed budget_exhausted = %v, want false", got)
	}
	if got := resp.Output["processed"]; got != 5 {
		t.Fatalf("resumed processed = %v, want 5", got)
	}
	if cursor := m.currentCursor(); cursor != "" {
		t.Fatalf("cursor after complete pass = %q, want empty", cursor)
	}
	// The resume must start at c and reach the items the first pass never got
	// to, then wrap for a full cycle.
	joined := ""
	for _, key := range attemptedSecond {
		joined += key
	}
	if joined != "cdeab" {
		t.Fatalf("second pass order = %q, want %q", joined, "cdeab")
	}
}

// TestRunPassDefersPoisonItem proves that a single item whose per-item bound
// expires is skipped rather than aborting the pass, and that items behind it
// still run.
func TestRunPassDefersPoisonItem(t *testing.T) {
	m := newPassTestMonitor(t)
	m.itemTimeout = 20 * time.Millisecond

	items := []monitoredMedia{
		movieQueueItem("a"),
		movieQueueItem("b"),
		movieQueueItem("c"),
	}
	m.evaluateFn = func(ctx context.Context, item monitoredMedia) (monitoredMedia, string, error) {
		if item.Key == "b" {
			<-ctx.Done()
			return item, "", ctx.Err()
		}
		return item, "", nil
	}

	resp, err := m.runPass(context.Background(), items, &trackingRegistrar{})
	if err != nil {
		t.Fatalf("pass with deferred item returned error: %v", err)
	}
	if got := resp.Output["budget_exhausted"]; got != false {
		t.Fatalf("budget_exhausted = %v, want false", got)
	}
	if got := resp.Output["processed"]; got != 3 {
		t.Fatalf("processed = %v, want 3 (c runs after b's timeout)", got)
	}
	if got := resp.Output["deferred"]; got != 1 {
		t.Fatalf("deferred = %v, want 1", got)
	}
	if got := resp.Output["pending"]; got != 3 {
		t.Fatalf("pending = %v, want 3", got)
	}
}

// TestRunPassReconcileFailureDoesNotFailPass proves a failing source
// reconciliation is logged and retried later, not returned as a pass error.
func TestRunPassReconcileFailureDoesNotFailPass(t *testing.T) {
	m := newPassTestMonitor(t)
	items := []monitoredMedia{movieQueueItem("a"), movieQueueItem("b")}
	m.evaluateFn = func(_ context.Context, item monitoredMedia) (monitoredMedia, string, error) {
		return item, "", nil
	}
	registrar := &trackingRegistrar{reconcileErr: errors.New("sweep failed")}

	resp, err := m.runPass(context.Background(), items, registrar)
	if err != nil {
		t.Fatalf("runPass returned error for a per-source reconcile failure: %v", err)
	}
	if got := resp.Output["processed"]; got != 2 {
		t.Fatalf("processed = %v, want 2", got)
	}
	if len(registrar.reconcileCalls) != 2 {
		t.Fatalf("reconcile calls = %v, want both sources", registrar.reconcileCalls)
	}
}

// TestMonitorConfigRoundTripsCursorAndLegacyArray verifies the persisted cursor
// survives a reload and that a queue file written by an older release (a bare
// JSON array) still loads, with no resume position.
func TestMonitorConfigRoundTripsCursorAndLegacyArray(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.json")

	m := newPassTestMonitor(t)
	m.config.File = path
	if err := m.remember(movieQueueItem("a")); err != nil {
		t.Fatalf("remember a: %v", err)
	}
	if err := m.remember(movieQueueItem("b")); err != nil {
		t.Fatalf("remember b: %v", err)
	}
	if err := m.setCursor("b"); err != nil {
		t.Fatalf("setCursor: %v", err)
	}

	reloaded := newMediaMonitor(nil, nil)
	if err := reloaded.Configure(Config{File: path, ProwlarrIndexFile: filepath.Join(dir, "index.json")}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if got := reloaded.currentCursor(); got != "b" {
		t.Fatalf("reloaded cursor = %q, want %q", got, "b")
	}
	if _, ok := reloaded.item("a"); !ok {
		t.Fatal("reloaded queue lost item a")
	}
	if _, ok := reloaded.item("b"); !ok {
		t.Fatal("reloaded queue lost item b")
	}

	// Legacy shape: a bare array has no cursor and must still load.
	legacy := filepath.Join(dir, "legacy.json")
	legacyItems := []monitoredMedia{movieQueueItem("old")}
	data, err := json.Marshal(legacyItems)
	if err != nil {
		t.Fatalf("marshal legacy queue: %v", err)
	}
	if err := os.WriteFile(legacy, data, 0o600); err != nil {
		t.Fatalf("write legacy queue: %v", err)
	}
	fromLegacy := newMediaMonitor(nil, nil)
	if err := fromLegacy.Configure(Config{File: legacy, ProwlarrIndexFile: filepath.Join(dir, "index.json")}); err != nil {
		t.Fatalf("Configure legacy: %v", err)
	}
	if got := fromLegacy.currentCursor(); got != "" {
		t.Fatalf("legacy cursor = %q, want empty", got)
	}
	if _, ok := fromLegacy.item("old"); !ok {
		t.Fatal("legacy queue lost item old")
	}
}
