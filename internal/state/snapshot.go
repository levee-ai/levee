// Package state persists budget-store snapshots as atomic JSON files and
// restores them on startup.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/levee-ai/levee/internal/budget"
)

// currentVersion is the snapshot file schema version. Additive fields stay at
// this version (unknown fields are ignored on read). A bump is reserved for
// incompatible changes. A file with any OTHER version refuses startup: it
// usually means a binary downgrade, and zeroing the ledger for it would be
// wrong.
const currentVersion = 1

// fileEnvelope is the on-disk shape. The envelope (version, written_at) is
// owned by this package, the per-agent payload types by the budget package.
type fileEnvelope struct {
	Version   int                             `json:"version"`
	WrittenAt time.Time                       `json:"written_at"`
	Agents    map[string]budget.AgentSnapshot `json:"agents"`
}

// LoadResult is what Load found on disk.
type LoadResult struct {
	Agents       map[string]budget.AgentSnapshot
	WrittenAt    time.Time
	Fresh        bool   // no prior state (file missing or empty)
	CorruptAside string // non-empty: corrupt file moved here, state is fresh
}

// Load reads the snapshot file. The taxonomy, per the Session 8 design:
// missing or empty file is a first run (fresh). Unparseable content is
// corrupt: it is renamed aside (preserving the evidence past the next write)
// and the result is fresh. An unknown version REFUSES (error). Any other
// read error REFUSES: zeroing the ledger over a transient permission or I/O
// error while good durable state exists would refill every budget, and
// fail-safe means refusing.
func Load(path string, now func() time.Time) (LoadResult, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return LoadResult{Fresh: true}, nil
	}
	if err != nil {
		return LoadResult{}, fmt.Errorf("state: reading %s: %w", path, err)
	}
	if len(raw) == 0 {
		return LoadResult{Fresh: true}, nil
	}

	var envelope fileEnvelope
	if unmarshalErr := json.Unmarshal(raw, &envelope); unmarshalErr != nil {
		asidePath := fmt.Sprintf("%s.corrupt-%d", path, now().Unix())
		if renameErr := os.Rename(path, asidePath); renameErr != nil {
			return LoadResult{}, fmt.Errorf("state: %s is corrupt and could not be moved aside: %w", path, renameErr)
		}
		return LoadResult{Fresh: true, CorruptAside: asidePath}, nil
	}
	if envelope.Version != currentVersion {
		return LoadResult{}, fmt.Errorf("state: %s has unknown version %d (this binary writes version %d), refusing to start",
			path, envelope.Version, currentVersion)
	}
	return LoadResult{Agents: envelope.Agents, WrittenAt: envelope.WrittenAt}, nil
}

// Snapshotter periodically writes the store's committed usage to disk.
type Snapshotter struct {
	store    *budget.Store
	path     string
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time

	// syncFile is swappable so tests can inject an fsync failure and assert
	// the abort-before-rename contract.
	syncFile func(*os.File) error

	cancel          context.CancelFunc
	done            chan struct{}
	lastSuccessUnix atomic.Int64
}

// NewSnapshotter builds a Snapshotter. Start begins the loop, Stop joins it.
func NewSnapshotter(store *budget.Store, path string, interval time.Duration, logger *slog.Logger) *Snapshotter {
	return &Snapshotter{
		store:    store,
		path:     path,
		interval: interval,
		logger:   logger,
		now:      time.Now,
		syncFile: func(file *os.File) error { return file.Sync() },
	}
}

// ProbeWritable verifies the snapshot directory accepts writes (create and
// remove a probe file) and sweeps stale temp files from prior crashes. It
// runs once at serve start, and failure refuses startup: a proxy that cannot
// persist should say so before taking traffic, not discover it at the first
// tick.
func (snapshotter *Snapshotter) ProbeWritable() error {
	directory := filepath.Dir(snapshotter.path)
	probe, err := os.CreateTemp(directory, filepath.Base(snapshotter.path)+".probe-*")
	if err != nil {
		return fmt.Errorf("state: snapshot directory %s is not writable: %w", directory, err)
	}
	probeName := probe.Name()
	if closeErr := probe.Close(); closeErr != nil {
		return fmt.Errorf("state: closing probe file: %w", closeErr)
	}
	if removeErr := os.Remove(probeName); removeErr != nil {
		return fmt.Errorf("state: removing probe file: %w", removeErr)
	}

	staleTemps, globErr := filepath.Glob(filepath.Join(directory, filepath.Base(snapshotter.path)+".tmp-*"))
	if globErr != nil {
		return fmt.Errorf("state: sweeping stale temp files: %w", globErr)
	}
	for _, staleTemp := range staleTemps {
		if removeErr := os.Remove(staleTemp); removeErr != nil {
			snapshotter.logger.Warn("Stale snapshot temp file could not be removed", "path", staleTemp, "error", removeErr.Error())
		}
	}
	return nil
}

// Start launches the periodic write loop. Call after Restore and before the
// listeners, so a first tick can never persist pre-restore fresh state over a
// good file.
func (snapshotter *Snapshotter) Start() {
	loopContext, cancel := context.WithCancel(context.Background())
	snapshotter.cancel = cancel
	snapshotter.done = make(chan struct{})
	go func() {
		defer close(snapshotter.done)
		ticker := time.NewTicker(snapshotter.interval)
		defer ticker.Stop()
		for {
			select {
			case <-loopContext.Done():
				return
			case <-ticker.C:
				if err := snapshotter.WriteOnce(); err != nil {
					snapshotter.logger.Error("State snapshot write failed", "path", snapshotter.path, "error", err.Error())
				}
			}
		}
	}()
}

// Stop cancels the loop and JOINS the goroutine. After Stop returns, no
// ticker write is running or can start, so the caller's final WriteOnce
// cannot race an in-flight older write.
func (snapshotter *Snapshotter) Stop() {
	if snapshotter.cancel == nil {
		return
	}
	snapshotter.cancel()
	<-snapshotter.done
}

// LastSuccess reports the time of the last successful write, for /health.
func (snapshotter *Snapshotter) LastSuccess() (time.Time, bool) {
	unixSeconds := snapshotter.lastSuccessUnix.Load()
	if unixSeconds == 0 {
		return time.Time{}, false
	}
	return time.Unix(unixSeconds, 0).UTC(), true
}

// WriteOnce exports the store and atomically replaces the snapshot file:
// unique temp file in the same directory, write, fsync, close, rename over
// the target, then best-effort directory fsync. Any step failure aborts
// BEFORE the rename and removes the temp file, so a file of unknown
// durability never replaces a known-good one.
func (snapshotter *Snapshotter) WriteOnce() error {
	envelope := fileEnvelope{
		Version:   currentVersion,
		WrittenAt: snapshotter.now().UTC(),
		Agents:    snapshotter.store.Export(),
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("state: encoding snapshot: %w", err)
	}

	directory := filepath.Dir(snapshotter.path)
	temporary, err := os.CreateTemp(directory, filepath.Base(snapshotter.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("state: creating temp file: %w", err)
	}
	temporaryName := temporary.Name()
	abort := func(step string, stepErr error) error {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
		return fmt.Errorf("state: %s: %w", step, stepErr)
	}
	if _, writeErr := temporary.Write(encoded); writeErr != nil {
		return abort("writing temp file", writeErr)
	}
	if syncErr := snapshotter.syncFile(temporary); syncErr != nil {
		return abort("fsync temp file", syncErr)
	}
	if closeErr := temporary.Close(); closeErr != nil {
		_ = os.Remove(temporaryName)
		return fmt.Errorf("state: closing temp file: %w", closeErr)
	}
	if renameErr := os.Rename(temporaryName, snapshotter.path); renameErr != nil {
		_ = os.Remove(temporaryName)
		return fmt.Errorf("state: renaming snapshot into place: %w", renameErr)
	}
	if directoryHandle, openErr := os.Open(directory); openErr == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}

	snapshotter.lastSuccessUnix.Store(snapshotter.now().Unix())
	return nil
}
