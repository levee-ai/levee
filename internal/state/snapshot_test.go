package state

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	report, restoreErr := restored.Restore(result.Agents, result.PausedAgents)
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
	if err := snapshotter.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
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

func TestSnapshotter_StartSecondCallErrorsAndStopStillJoinsTheLoop(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	snapshotter := NewSnapshotter(testStore(t), path, 10*time.Millisecond, testLogger())

	if err := snapshotter.Start(); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := snapshotter.Start(); err == nil {
		t.Fatal("second Start must error, Start is once per Snapshotter")
	}

	time.Sleep(35 * time.Millisecond)
	snapshotter.Stop()

	// If the reentry guard did not exist, the second Start call would have
	// overwritten cancel and done, leaking the first loop's goroutine: Stop
	// would join only the second (never-started-for-real) handles, and the
	// leaked first goroutine could keep writing forever. Assert the same
	// no-write-after-Stop invariant as the single-Start case.
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
		t.Fatal("a write occurred after Stop returned: a second Start leaked the first loop's goroutine")
	}
}

func TestWriteOncePersistsPausedAgentsAndLoadReturnsThem(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	store := testStore(t)
	if err := store.SetPaused("agent-a", true); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	snapshotter := NewSnapshotter(store, path, time.Minute, testLogger())
	if err := snapshotter.WriteOnce(); err != nil {
		t.Fatalf("WriteOnce: %v", err)
	}

	result, err := Load(path, time.Now)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(result.PausedAgents) != 1 || result.PausedAgents[0] != "agent-a" {
		t.Fatalf("PausedAgents = %v, want [agent-a]", result.PausedAgents)
	}
}

func TestLoadOldFileWithoutPausedAgents(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	oldFile := `{"version":1,"written_at":"2026-09-11T00:00:00Z","agents":{}}`
	if err := os.WriteFile(path, []byte(oldFile), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	result, err := Load(path, time.Now)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(result.PausedAgents) != 0 {
		t.Fatalf("PausedAgents = %v, want empty", result.PausedAgents)
	}
}

// TestWriteOnce_SerializesStaleExportBehindMutation pins the ordering
// property the writeMutex exists for: a writer that exported BEFORE a state
// mutation must not rename its stale envelope over a file written AFTER the
// mutation. The first writer is held inside syncFile (after exporting the
// pre-pause envelope), the agent is paused, a second WriteOnce runs, then
// the first writer is released. Serialized, the second writer waits on the
// mutex and writes last, so the surviving file carries the pause.
// Unserialized, the first writer renames its stale envelope over the
// pause-bearing file.
func TestWriteOnce_SerializesStaleExportBehindMutation(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	store := testStore(t)
	snapshotter := NewSnapshotter(store, path, time.Minute, testLogger())

	firstWriterInSync := make(chan struct{})
	releaseFirstWriter := make(chan struct{})
	secondWriterInSync := make(chan struct{})
	var syncCallCount atomic.Int64
	snapshotter.syncFile = func(file *os.File) error {
		switch syncCallCount.Add(1) {
		case 1:
			close(firstWriterInSync)
			<-releaseFirstWriter
		case 2:
			close(secondWriterInSync)
		}
		return file.Sync()
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- snapshotter.WriteOnce() }()
	<-firstWriterInSync

	// The mutation lands after the first writer already exported.
	if pauseErr := store.SetPaused("agent-a", true); pauseErr != nil {
		t.Fatalf("SetPaused: %v", pauseErr)
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- snapshotter.WriteOnce() }()

	// Serialized, the second writer parks on the mutex before exporting
	// anything, so it cannot reach syncFile and the timer fires. Without
	// serialization it reaches syncFile while the first writer is still
	// held, and the test then JOINS its completion before releasing the
	// first writer, so the stale rename deterministically lands last. The
	// timer only sets green-run duration, never the verdict.
	secondFinishedFirst := false
	select {
	case <-secondWriterInSync:
		if secondErr := <-secondDone; secondErr != nil {
			t.Fatalf("second WriteOnce: %v", secondErr)
		}
		secondFinishedFirst = true
	case <-time.After(300 * time.Millisecond):
	}

	close(releaseFirstWriter)
	if firstErr := <-firstDone; firstErr != nil {
		t.Fatalf("first WriteOnce: %v", firstErr)
	}
	if !secondFinishedFirst {
		select {
		case secondErr := <-secondDone:
			if secondErr != nil {
				t.Fatalf("second WriteOnce: %v", secondErr)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("second WriteOnce did not finish after the first writer released")
		}
	}

	result, loadErr := Load(path, time.Now)
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if len(result.PausedAgents) != 1 || result.PausedAgents[0] != "agent-a" {
		t.Fatalf("stale envelope overwrote the pause: PausedAgents = %v, want [agent-a]", result.PausedAgents)
	}
}

// TestWriteOnce_ConcurrentCallersRaceClean is a race-detector smoke test:
// it hammers the real write path (temp create, fsync, rename, last-success
// stores) from eight goroutines. It does not pin serialization ordering,
// TestWriteOnce_SerializesStaleExportBehindMutation does.
func TestWriteOnce_ConcurrentCallersRaceClean(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	store := testStore(t)
	snapshotter := NewSnapshotter(store, path, time.Minute, testLogger())

	var waitGroup sync.WaitGroup
	for i := 0; i < 8; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for j := 0; j < 20; j++ {
				if err := snapshotter.WriteOnce(); err != nil {
					t.Errorf("concurrent WriteOnce: %v", err)
					return
				}
			}
		}()
	}
	waitGroup.Wait()

	if _, err := Load(path, time.Now); err != nil {
		t.Fatalf("Load after concurrent writes: %v", err)
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
