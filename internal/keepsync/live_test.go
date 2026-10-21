package keepsync

import (
	"bytes"
	"context"
	"encoding/json"
	"go.etcd.io/bbolt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveUploadsLogsDeletionAndRestart(t *testing.T) {
	c := testConfig(t)
	c.LogFormat = "json"
	var logs bytes.Buffer
	s := New(c, &logs, io.Discard)
	ctx := context.Background()
	run := func(command string) {
		t.Helper()
		if err := s.Run(ctx, []string{command}); err != nil {
			t.Fatalf("%s: %v", command, err)
		}
	}
	file := filepath.Join(c.Source, "FILEA.JSON")
	if err := os.WriteFile(file, bytes.Repeat([]byte("a"), 1125), 0600); err != nil {
		t.Fatal(err)
	}
	run("init")
	run("register-source")
	run("backup")
	if _, err := os.Stat(filepath.Join(c.Repository, "Snapshots")); !os.IsNotExist(err) {
		t.Fatal("disabled snapshots created history")
	}
	if err := os.WriteFile(file, bytes.Repeat([]byte("b"), 5500), 0600); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	run("backup")
	found := false
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["msg"] == "FILE uploaded" {
			found = true
			if event["path"] != "FILEA.JSON" || event["old_bytes"] != float64(1125) || event["bytes"] != float64(5500) {
				t.Fatalf("incorrect file log: %s", line)
			}
		}
	}
	if !found {
		t.Fatal("missing confirmed upload event")
	}
	liveFile := filepath.Join(c.Repository, "Live", "FILEA.JSON")
	data, err := os.ReadFile(liveFile)
	if err != nil || len(data) != 5500 {
		t.Fatal("latest file missing from Live")
	}
	logs.Reset()
	s = New(c, &logs, io.Discard)
	run("backup")
	if strings.Contains(logs.String(), "FILE uploaded") {
		t.Fatal("unchanged file uploaded after restart")
	}
	// The fallback also repairs a remote object lost outside KeepSync.
	if err := os.Remove(liveFile); err != nil {
		t.Fatal(err)
	}
	run("backup")
	if _, err := os.Stat(liveFile); err != nil {
		t.Fatal("missing remote object not repaired")
	}
	if err := os.WriteFile(filepath.Join(c.Source, "keep.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	run("backup")
	if _, err := os.Stat(liveFile); err != nil {
		t.Fatal("default deletion policy lost remote backup")
	}
	// Re-add then explicitly opt in to deletion propagation.
	if err := os.WriteFile(file, []byte("again"), 0600); err != nil {
		t.Fatal(err)
	}
	run("backup")
	s.cfg.DeleteRemote = true
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	run("backup")
	if _, err := os.Stat(liveFile); !os.IsNotExist(err) {
		t.Fatal("explicit deletion did not remove Live file")
	}
	if err := os.WriteFile(s.statePath(".stage-interrupted"), []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	run("backup")
	if _, err := os.Stat(s.statePath(".stage-interrupted")); !os.IsNotExist(err) {
		t.Fatal("interrupted staging file not cleaned")
	}
}

func TestInvalidIndexCannotDeleteSnapshots(t *testing.T) {
	c := testConfig(t)
	c.DeleteRemote = true
	s := New(c, io.Discard, io.Discard)
	if err := os.WriteFile(filepath.Join(c.Source, "file"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background(), []string{"register-source"}); err != nil {
		t.Fatal(err)
	}
	index, err := openIndex(s.statePath("live-index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := index.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(filesBucket).Put([]byte("../Snapshots/config"), []byte(`{"size":1}`))
	}); err != nil {
		t.Fatal(err)
	}
	if err := index.close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background(), []string{"backup"}); err == nil {
		t.Fatal("accepted escaping index path")
	}
}

func TestLiveWatcherHandlesNewDirectories(t *testing.T) {
	c := testConfig(t)
	c.Method = "live"
	c.Interval = "1h"
	c.Debounce = "1s"
	c.MaxWait = "2s"
	s := New(c, io.Discard, io.Discard)
	if err := os.WriteFile(filepath.Join(c.Source, "first.txt"), []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background(), []string{"register-source"}); err != nil {
		t.Fatal(err)
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
			t.Error("service did not stop")
		}
	}()
	waitFor(t, 10*time.Second, func() bool { _, err := os.Stat(filepath.Join(c.Repository, "Live", "first.txt")); return err == nil })
	dir := filepath.Join(c.Source, "new", "nested")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "new.json")
	if err := os.WriteFile(file, []byte("live"), 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(c.Repository, "Live", "new", "nested", "new.json"))
		return err == nil && string(data) == "live"
	})
	if err := os.WriteFile(file, []byte("updated live"), 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(c.Repository, "Live", "new", "nested", "new.json"))
		return err == nil && string(data) == "updated live"
	})
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}

func TestBatchQuietPeriodAndMaxWait(t *testing.T) {
	start := time.Now()
	var b batch
	b.changed(start)
	if b.due(start.Add(500*time.Millisecond), time.Second, 3*time.Second) {
		t.Fatal("backup before debounce")
	}
	b.changed(start.Add(900 * time.Millisecond))
	if b.due(start.Add(1500*time.Millisecond), time.Second, 3*time.Second) {
		t.Fatal("quiet period did not reset")
	}
	if !b.due(start.Add(2*time.Second), time.Second, 3*time.Second) {
		t.Fatal("quiet batch was not due")
	}
	b.changed(start.Add(2900 * time.Millisecond))
	if !b.due(start.Add(3*time.Second), time.Second, 3*time.Second) {
		t.Fatal("continuous writes postponed backup beyond max_wait")
	}
}

func TestReconciliationDetectsPreservedMtime(t *testing.T) {
	c := testConfig(t)
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	file := filepath.Join(c.Source, "same-size.txt")
	if err := os.WriteFile(file, []byte("aaaa"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"register-source", "backup"} {
		if err := s.Run(ctx, []string{command}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(file, []byte("bbbb"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(c.Repository, "Live", "same-size.txt")); err != nil || string(b) != "bbbb" {
		t.Fatal("metadata cache skipped changed content")
	}
}
