package state

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levee-ai/levee/internal/budget"
	"github.com/levee-ai/levee/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testStore(t *testing.T) *budget.Store {
	t.Helper()
	agents := []config.AgentConfig{{
		Name: "agent-a",
		Mode: "enforce",
		Budgets: []config.BudgetConfig{
			{Type: "tokens", Limit: 1000, Window: "1h", WindowType: "rolling"},
		},
	}}
	store, err := budget.NewStore(agents, budget.DefaultStreamLimit, nil)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return store
}

func TestLoad_MissingAndEmptyMeanFresh(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")

	result, err := Load(path, time.Now)
	if err != nil || !result.Fresh {
		t.Fatalf("missing file: fresh=%v err=%v, want fresh with no error", result.Fresh, err)
	}

	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err = Load(path, time.Now)
	if err != nil || !result.Fresh {
		t.Fatalf("empty file: fresh=%v err=%v, want fresh with no error", result.Fresh, err)
	}
}

func TestLoad_CorruptFileGoesFreshAndIsRenamedAside(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Load(path, time.Now)
	if err != nil {
		t.Fatalf("corrupt must not error: %v", err)
	}
	if !result.Fresh || result.CorruptAside == "" {
		t.Fatalf("result = %+v, want fresh with a corrupt-aside path", result)
	}
	if _, statErr := os.Stat(result.CorruptAside); statErr != nil {
		t.Fatalf("aside file missing: %v", statErr)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("original corrupt file should have been moved aside")
	}
}

func TestLoad_UnknownVersionRefuses(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	body := `{"version": 2, "written_at": "2026-09-09T12:00:00Z", "agents": {}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, time.Now)
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("unknown version must refuse startup, err = %v", err)
	}
}

func TestLoad_ReadErrorRefuses(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission checks do not bind for root")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"agents":{}}`), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, time.Now)
	if err == nil {
		t.Fatal("unreadable file must refuse startup, not zero the ledger")
	}
}

func TestSnapshotter_WriteOnceRoundTripsThroughLoad(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	store := testStore(t)
	if err := store.TrackMulti("agent-a", []int64{123}); err != nil {
		t.Fatal(err)
	}

	snapshotter := NewSnapshotter(store, path, time.Minute, testLogger())
	if err := snapshotter.WriteOnce(); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := Load(path, time.Now)
	if err != nil || result.Fresh {
		t.Fatalf("load after write: fresh=%v err=%v", result.Fresh, err)
	}
	restored := testStore(t)
	report, restoreErr := restored.Restore(result.Agents)
	if restoreErr != nil {
		t.Fatalf("restore: %v", restoreErr)
	}
	if report.RestoredBudgets != 1 {
		t.Fatalf("report = %+v", report)
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temp files leaked: %v", entries)
	}

	lastSuccess, ok := snapshotter.LastSuccess()
	if !ok || lastSuccess.IsZero() {
		t.Fatal("LastSuccess must report the successful write")
	}
}

func TestSnapshotter_SyncFailureAbortsBeforeRename(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	good := []byte(`{"version":1,"written_at":"2026-09-09T12:00:00Z","agents":{}}`)
	if err := os.WriteFile(path, good, 0o644); err != nil {
		t.Fatal(err)
	}

	snapshotter := NewSnapshotter(testStore(t), path, time.Minute, testLogger())
	snapshotter.syncFile = func(file *os.File) error { return errors.New("injected fsync failure") }
	if err := snapshotter.WriteOnce(); err == nil {
		t.Fatal("expected WriteOnce to fail on fsync error")
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(good) {
		t.Fatal("a failed fsync must never replace the known-good file")
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 1 {
		t.Fatalf("failed write must remove its temp file: %v", entries)
	}
}

func TestSnapshotter_ProbeWritableAndSweep(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	stale := filepath.Join(directory, "state.json.tmp-123456")
	if err := os.WriteFile(stale, []byte("leftover"), 0o644); err != nil {
		t.Fatal(err)
	}

	snapshotter := NewSnapshotter(testStore(t), path, time.Minute, testLogger())
	if err := snapshotter.ProbeWritable(); err != nil {
		t.Fatalf("probe on a writable directory: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale temp file should have been swept")
	}

	if os.Getuid() != 0 {
		readOnly := filepath.Join(t.TempDir(), "readonly")
		if err := os.Mkdir(readOnly, 0o555); err != nil {
			t.Fatal(err)
		}
		blocked := NewSnapshotter(testStore(t), filepath.Join(readOnly, "state.json"), time.Minute, testLogger())
		if err := blocked.ProbeWritable(); err == nil {
			t.Fatal("probe must fail on an unwritable directory")
		}
	}
}

func TestSnapshotter_StopJoinsTheLoop(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	snapshotter := NewSnapshotter(testStore(t), path, 10*time.Millisecond, testLogger())
	snapshotter.Start()
	time.Sleep(35 * time.Millisecond)
	snapshotter.Stop()

	// After Stop returns, no further write may occur. Capture the file's
	// modification time, wait several intervals, and assert it is unchanged.
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected at least one periodic write: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatal("a write occurred after Stop returned: the loop was not joined")
	}
}

func TestEnvelope_UnknownFieldsAreIgnored(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	body := map[string]any{
		"version":      1,
		"written_at":   "2026-09-09T12:00:00Z",
		"agents":       map[string]any{},
		"future_field": "ignored",
	}
	raw, _ := json.Marshal(body)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Load(path, time.Now)
	if err != nil || result.Fresh {
		t.Fatalf("additive fields must stay version 1 and load: fresh=%v err=%v", result.Fresh, err)
	}
}
