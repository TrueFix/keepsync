package keepsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	// Use canonical fixture paths on platforms where /var is a symlink.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	c := defaultConfig()
	c.Dataset = "test"
	c.Source = filepath.Join(root, "source")
	c.Repository = filepath.Join(root, "repo")
	c.StateDir = filepath.Join(root, "state")
	c.RestoreRoot = filepath.Join(root, "restore")
	c.Interval = "1s"
	for _, dir := range []string{c.Source, c.StateDir, c.RestoreRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestBackgroundLockWaitsAndCancels(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if release, err := lockStateContext(ctx, dir); err == nil {
		release()
		t.Fatal("cancelled lock unexpectedly acquired")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		release, err := lockStateContext(ctx, dir)
		if err == nil {
			release()
		}
		done <- err
	}()
	time.Sleep(40 * time.Millisecond)
	unlock()
	if err := <-done; err != nil {
		t.Fatal("background worker treated brief lock contention as failure:", err)
	}
}

func TestConfigRejectsUnsafeValues(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"overlapping state", func(c *Config) { c.StateDir = filepath.Join(c.Source, "state") }},
		{"overlapping restore", func(c *Config) { c.RestoreRoot = c.Source }},
		{"root source", func(c *Config) { c.Source = "/" }},
		{"relative source", func(c *Config) { c.Source = "data" }},
		{"invalid interval", func(c *Config) { c.Interval = "0s" }},
		{"invalid dataset", func(c *Config) { c.Dataset = "--bad" }},
		{"empty retention", func(c *Config) { c.Snapshots.KeepLast = 0 }},
		{"negative retention", func(c *Config) { c.Snapshots.KeepLast = -1 }},
		{"unknown method", func(c *Config) { c.Method = "auto" }},
		{"invalid debounce", func(c *Config) { c.Debounce = "0s" }},
		{"max wait shorter than debounce", func(c *Config) { c.MaxWait = "1s" }},
		{"invalid snapshot interval", func(c *Config) { c.Snapshots.Interval = "0s" }},
		{"invalid watch limit", func(c *Config) { c.MaxWatches = 0 }},
		{"invalid watch mode", func(c *Config) { c.WatchMode = "broken" }},
		{"invalid poll interval", func(c *Config) { c.WatchInterval = "0s" }},
		{"unknown snapshot format", func(c *Config) { c.Snapshots.Format = "zip" }},
		{"removed encrypted format", func(c *Config) { c.Snapshots.Format = "restic" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig(t)
			tt.mutate(&c)
			if c.Validate() == nil {
				t.Fatal("accepted unsafe configuration")
			}
		})
	}
}

func TestConfigStrictDecoding(t *testing.T) {
	for _, data := range []string{`{"datset":"typo"}`, `{} {}`, `{}`} {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(p); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestLockExcludesOtherOperationsAndReleases(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if release, err := lockState(dir); err == nil {
		release()
		t.Fatal("second lock succeeded")
	}
	unlock()
	release, err := lockState(dir)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestResolvedSymlinkOverlap(t *testing.T) {
	c := testConfig(t)
	link := filepath.Join(filepath.Dir(c.Source), "alias")
	if err := os.Symlink(c.Source, link); err != nil {
		t.Fatal(err)
	}
	c.RestoreRoot = filepath.Join(link, "new-restore")
	if err := New(c, nil, nil).safePaths(c.Source); err == nil {
		t.Fatal("symlink bypass accepted")
	}
}

func TestEmptySourceIgnoresMarker(t *testing.T) {
	c := testConfig(t)
	if err := os.WriteFile(filepath.Join(c.Source, markerName), []byte("marker"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := hasFiles(c.Source); err == nil {
		t.Fatal("marker counted as user data")
	}
}

func TestComposeEnvironmentConfiguration(t *testing.T) {
	for _, key := range []string{"KEEPSYNC_TARGET", "KEEPSYNC_DATASET", "KEEPSYNC_SOURCE_PATH", "KEEPSYNC_REPOSITORY", "KEEPSYNC_STATE_DIR", "KEEPSYNC_RESTORE_ROOT", "KEEPSYNC_INTERVAL", "KEEPSYNC_METHOD", "KEEPSYNC_DEBOUNCE", "KEEPSYNC_MAX_WAIT", "KEEPSYNC_SNAPSHOTS_ENABLED", "KEEPSYNC_SNAPSHOT_FORMAT", "KEEPSYNC_SNAPSHOT_INTERVAL", "KEEPSYNC_SNAPSHOT_RETAIN", "KEEPSYNC_LOG_LEVEL", "KEEPSYNC_LOG_FORMAT", "KEEPSYNC_LOG_FILES", "KEEPSYNC_DELETE_REMOTE", "KEEPSYNC_EXCLUDES", "KEEPSYNC_S3_ENDPOINT", "KEEPSYNC_MAX_WATCHES"} {
		old, exists := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if exists {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("accepted missing repository")
	}
	t.Setenv("KEEPSYNC_REPOSITORY", "s3:s3.amazonaws.com/bucket/files")
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Dataset != "files" || c.Source != "/data" || c.StateDir != "/var/lib/keepsync" || c.RestoreRoot != "/restore" || c.Interval != "1h" || !c.Snapshots.Enabled || c.Method != "live" || c.Snapshots.Format != "tar" || c.Snapshots.KeepLast != 14 {
		t.Fatalf("unexpected container defaults: %+v", c)
	}
	t.Setenv("KEEPSYNC_SOURCE_PATH", "/srv/files")
	t.Setenv("KEEPSYNC_DATASET", "app-files")
	t.Setenv("KEEPSYNC_INTERVAL", "30m")
	c, err = LoadConfig("")
	if err != nil || c.Source != "/srv/files" || c.Dataset != "app-files" || c.Interval != "30m" {
		t.Fatalf("environment overrides: %+v, %v", c, err)
	}
	t.Setenv("KEEPSYNC_STATE_DIR", "/srv/files/state")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("accepted overlapping paths from environment")
	}
	t.Setenv("KEEPSYNC_STATE_DIR", "/var/lib/keepsync")
	t.Setenv("KEEPSYNC_INTERVAL", "")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("accepted explicitly empty interval")
	}
}

func TestExplicitConfigDoesNotMixEnvironment(t *testing.T) {
	c := testConfig(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeJSON(path, c); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KEEPSYNC_REPOSITORY", "s3:s3.amazonaws.com/wrong-bucket/files")
	t.Setenv("KEEPSYNC_DATASET", "invalid dataset")
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Repository != c.Repository || loaded.Dataset != c.Dataset {
		t.Fatal("environment overrode an explicit configuration file")
	}
	if _, err := LoadConfig(path + ".missing"); err == nil {
		t.Fatal("missing explicit configuration fell back to environment")
	}
}
