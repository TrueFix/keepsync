package keepsync

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLiveS3CreateEditAtomicSaveRenameAndDeleteLastFile(t *testing.T) {
	for _, mode := range []string{"native", "poll"} {
		t.Run(mode, func(t *testing.T) {
			c, backend, _ := fakeS3(t)
			c.Method = "live"
			c.Interval = "1h"
			c.Debounce = "1s"
			c.MaxWait = "2s"
			c.WatchMode = mode
			c.WatchInterval = "1s"
			c.DeleteRemote = true
			dir := filepath.Join(c.Source, "nested")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, "file.json")
			if err := os.WriteFile(file, []byte("initial"), 0600); err != nil {
				t.Fatal(err)
			}
			s := New(c, io.Discard, io.Discard)
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
					t.Error("live service did not stop")
				}
			}()
			content := func(rel string) (string, bool) {
				obj, err := backend.GetObject("backups", "my-app/Live/"+rel, nil)
				if err != nil {
					return "", false
				}
				defer obj.Contents.Close()
				b, err := io.ReadAll(obj.Contents)
				return string(b), err == nil
			}
			waitContent := func(rel, body string) {
				t.Helper()
				waitFor(t, 10*time.Second, func() bool { got, ok := content(rel); return ok && got == body })
			}
			waitMissing := func(rel string) {
				t.Helper()
				waitFor(t, 10*time.Second, func() bool { _, ok := content(rel); return !ok })
			}
			waitContent("nested/file.json", "initial")
			if err := os.WriteFile(file, []byte("edited"), 0600); err != nil {
				t.Fatal(err)
			}
			waitContent("nested/file.json", "edited")
			atomic := filepath.Join(t.TempDir(), "replacement")
			if err := os.WriteFile(atomic, []byte("atomic save"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(atomic, file); err != nil {
				t.Fatal(err)
			}
			waitContent("nested/file.json", "atomic save")
			// Move an already-populated directory into the mounted tree.
			incoming := filepath.Join(t.TempDir(), "incoming")
			if err := os.MkdirAll(filepath.Join(incoming, "deep"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(incoming, "deep", "new.txt"), []byte("created"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(incoming, filepath.Join(c.Source, "incoming")); err != nil {
				t.Fatal(err)
			}
			waitContent("incoming/deep/new.txt", "created")
			if err := os.WriteFile(filepath.Join(c.Source, "incoming", "deep", "new.txt"), []byte("nested edit"), 0600); err != nil {
				t.Fatal(err)
			}
			waitContent("incoming/deep/new.txt", "nested edit")
			if err := os.RemoveAll(filepath.Join(c.Source, "incoming")); err != nil {
				t.Fatal(err)
			}
			waitMissing("incoming/deep/new.txt")
			renamed := filepath.Join(dir, "renamed.json")
			if err := os.Rename(file, renamed); err != nil {
				t.Fatal(err)
			}
			waitContent("nested/renamed.json", "atomic save")
			waitMissing("nested/file.json")
			if err := os.Remove(renamed); err != nil {
				t.Fatal(err)
			}
			waitMissing("nested/renamed.json")
		})
	}
}

func TestLastFileDeletionIsDetectedWhenRemoteCopiesAreKept(t *testing.T) {
	c := testConfig(t)
	c.Method = "live"
	c.Interval = "1h"
	c.Debounce = "1s"
	c.MaxWait = "2s"
	c.LogFormat = "json"
	file := filepath.Join(c.Source, "last.txt")
	if err := os.WriteFile(file, []byte("keep this backup"), 0600); err != nil {
		t.Fatal(err)
	}
	var logs safeLog
	s := New(c, &logs, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, []string{"serve"}) }()
	defer func() { cancel(); <-done }()
	waitFor(t, 10*time.Second, func() bool { return logs.count("FILE uploaded") > 0 })
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { return logs.count("FILE removed locally") > 0 })
	if b, err := os.ReadFile(filepath.Join(c.Repository, "Live", "last.txt")); err != nil || string(b) != "keep this backup" {
		t.Fatal("default deletion policy discarded remote backup")
	}
}

func TestBusyFilesRetryWithoutMinuteBackoff(t *testing.T) {
	c := testConfig(t)
	c.Method = "live"
	c.Interval = "1h"
	c.Debounce = "1s"
	c.MaxWait = "2s"
	s := New(c, io.Discard, io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	changes := make(chan time.Time)
	completed := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.runLoop(ctx, changes, func(ctx context.Context, command string) error {
			if command == "backup" {
				s.pending.add("busy.json", false)
				return errFileBusy
			}
			if command == "changes" {
				close(completed)
				cancel()
			}
			return nil
		})
	}()
	select {
	case <-completed:
	case <-time.After(4 * time.Second):
		t.Fatal("busy file was delayed behind global retry backoff")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
