package keepsync

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type safeLog struct {
	mu   sync.Mutex
	text strings.Builder
}

func TestSnapshotWorkCannotStarveLiveChanges(t *testing.T) {
	c := testConfig(t)
	c.Method = "live"
	c.Interval = "1h"
	c.Debounce = "1s"
	c.MaxWait = "1s"
	c.Snapshots.Enabled = true
	s := New(c, io.Discard, io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	changes := make(chan time.Time)
	snapshotStarted := make(chan struct{})
	release := make(chan struct{})
	changed := make(chan struct{})
	done := make(chan error, 1)
	var snapshots atomic.Int32
	go func() {
		done <- s.runLoop(ctx, changes, func(ctx context.Context, command string) error {
			switch command {
			case "snapshot":
				if snapshots.Add(1) == 1 {
					close(snapshotStarted)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			case "changes":
				close(changed)
				cancel()
			}
			return nil
		})
	}()
	select {
	case <-snapshotStarted:
	case <-ctx.Done():
		t.Fatal("snapshot was not scheduled")
	}
	changes <- time.Now().Add(-2 * time.Second)
	close(release)
	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("Live changes starved behind snapshot work")
	}
	if snapshots.Load() != 1 {
		t.Fatal("another snapshot ran before the pending Live batch")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func (l *safeLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}
func (l *safeLog) count(text string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.text.String(), text)
}

func TestSnapshotsRunWithoutLiveChanges(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		if os.Getenv("KEEPSYNC_REQUIRE_INTEGRATION") == "1" {
			t.Fatal(err)
		}
		t.Skip("restic not installed")
	}
	c := testConfig(t)
	c.Method = "interval"
	c.Interval = "1h"
	c.Snapshots = SnapshotConfig{Enabled: true, Interval: "1s", KeepLast: 2}
	var logs safeLog
	s := New(c, &logs, io.Discard)
	if err := os.WriteFile(filepath.Join(c.Source, "file"), []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"init", "register-source"} {
		if err := s.Run(context.Background(), []string{command}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, []string{"serve"}) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("scheduler did not stop")
		}
	}()
	waitFor(t, 30*time.Second, func() bool { return logs.count("Snapshot retention complete") >= 2 })
	if logs.count("Live backup complete") != 1 {
		t.Fatal("snapshot schedule triggered extra Live reconciliations")
	}
}

func TestChangedBatchIsScopedAndBounded(t *testing.T) {
	c := testConfig(t)
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(c.Source, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range []string{"register-source", "backup"} {
		if err := s.Run(ctx, []string{command}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(c.Source, "a.txt"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	s.pending.add("a.txt", false)
	if err := s.backupChanges(ctx); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(c.Repository, "Live", "b.txt")); err != nil || string(b) != "b.txt" {
		t.Fatal("partial batch changed an unrelated object")
	}
	if err := os.Remove(filepath.Join(c.Source, "a.txt")); err != nil {
		t.Fatal(err)
	}
	s.cfg.DeleteRemote = true
	s.pending.add("a.txt", false)
	if err := s.backupChanges(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Repository, "Live", "a.txt")); !os.IsNotExist(err) {
		t.Fatal("scoped deletion failed")
	}
	var changes changeSet
	changes.add("dir", false)
	changes.add("dir-sibling", false)
	changes.add("dir/file", false)
	paths, full := changes.take()
	if full || len(paths) != 2 {
		t.Fatalf("overlapping scopes not compacted: %v", paths)
	}
	for i := 0; i < 5000; i++ {
		changes.add(time.Unix(int64(i), 0).Format(time.RFC3339), false)
	}
	paths, full = changes.take()
	if !full || len(paths) != 0 {
		t.Fatal("unbounded changed-path queue")
	}
}

func TestSnapshotProgressMemoryIsBounded(t *testing.T) {
	var output snapshotOutput
	for i := 0; i < 10000; i++ {
		_, _ = output.Write([]byte("{\"message_type\":\"status\",\"percent_done\":0.5}\n"))
	}
	_, _ = output.Write([]byte("{\"message_type\":\"summary\",\"snapshot_id\":\"abc123\",\"data_added_packed\":20}\n"))
	if output.err != nil || output.summary.SnapshotID != "abc123" || cap(output.line) > 1<<20 {
		t.Fatal("progress collector grew with backup duration")
	}
	var capped cappedOutput
	capped.limit = 32
	_, _ = capped.Write([]byte(strings.Repeat("x", 1000)))
	if len(capped.data) != 32 || !capped.truncated {
		t.Fatal("error output is not bounded")
	}
}
