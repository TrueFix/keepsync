package keepsync

import (
	"os"
	"path/filepath"
	"testing"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	// macOS /var is a symlink to /private/var. Restic stores the canonical
	// source path, so fixtures must use that same path on every platform.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	c := Config{Dataset: "test", Source: filepath.Join(root, "source"), Repository: filepath.Join(root, "repo"), PasswordFile: filepath.Join(root, "password"), StateDir: filepath.Join(root, "state"), RestoreRoot: filepath.Join(root, "restore"), Interval: "1s", Restic: "restic"}
	for _, dir := range []string{c.Source, c.StateDir, c.RestoreRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(c.PasswordFile, []byte("integration-test-password"), 0600); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConfigRejectsUnsafeValues(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"overlapping state", func(c *Config) { c.StateDir = filepath.Join(c.Source, "state") }},
		{"overlapping restore", func(c *Config) { c.RestoreRoot = c.Source }},
		{"password in source", func(c *Config) { c.PasswordFile = filepath.Join(c.Source, "password") }},
		{"root source", func(c *Config) { c.Source = "/" }},
		{"relative source", func(c *Config) { c.Source = "data" }},
		{"invalid interval", func(c *Config) { c.Interval = "0s" }},
		{"invalid dataset", func(c *Config) { c.Dataset = "--bad" }},
		{"empty retention", func(c *Config) { c.Retention.Enabled = true }},
		{"negative retention", func(c *Config) { c.Retention.KeepDaily = -1 }},
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
