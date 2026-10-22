package keepsync

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise automatic setup, archive recovery, and ownership guards together.
func TestIntegrationBackupRecoveryAndGuards(t *testing.T) {
	c := testConfig(t)
	c.Snapshots.Enabled = true
	for key, value := range map[string]string{
		"KEEPSYNC_DATASET":           c.Dataset,
		"KEEPSYNC_SOURCE_PATH":       c.Source,
		"KEEPSYNC_TARGET":            c.Repository,
		"KEEPSYNC_STATE_DIR":         c.StateDir,
		"KEEPSYNC_RESTORE_ROOT":      c.RestoreRoot,
		"KEEPSYNC_INTERVAL":          c.Interval,
		"KEEPSYNC_SNAPSHOTS_ENABLED": "true",
	} {
		t.Setenv(key, value)
	}
	var err error
	c, err = LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	s := New(c, &output, &output)
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		output.Reset()
		if err := s.Run(ctx, args); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, output.String())
		}
	}
	fail := func(args ...string) {
		t.Helper()
		if err := s.Run(ctx, args); err == nil {
			t.Fatalf("unsafe operation succeeded: %v", args)
		}
	}
	file := filepath.Join(c.Source, "important.txt")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	run("backup") // First use initializes state without writing to the source.
	fail("register-source")
	run("snapshot")
	run("snapshots")
	var list []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(output.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("snapshots: %s, %v", output.String(), err)
	}
	id := list[0].ID
	if err := os.WriteFile(file, []byte("newest"), 0600); err != nil {
		t.Fatal(err)
	}
	// A fresh controller models a restart: back up current data, never restore.
	s = New(c, &output, &output)
	run("backup")
	if data, _ := os.ReadFile(file); string(data) != "newest" {
		t.Fatal("restart modified source")
	}
	run("status")
	var status Status
	if err := json.Unmarshal(output.Bytes(), &status); err != nil || status.LastSuccess.IsZero() || status.LastError != "" {
		t.Fatalf("bad status: %s", output.String())
	}
	s.cfg.Snapshots.Enabled = false
	fail("prune")
	s.cfg.Snapshots.Enabled = true
	fail("restore", "--snapshot", "latest", "--target", filepath.Join(c.RestoreRoot, "bad"))
	fail("restore", "--snapshot", id, "--target", c.Source)
	fail("restore", "--snapshot", id, "--target", c.RestoreRoot)
	target := filepath.Join(c.RestoreRoot, "recovered")
	run("restore", "--snapshot", id, "--target", target)
	recovered := filepath.Join(target, "data", "important.txt")
	if data, err := os.ReadFile(recovered); err != nil || string(data) != "original" {
		t.Fatalf("restore: %q, %v", data, err)
	}
	fail("restore", "--snapshot", id, "--target", target)
	// Wrong dataset must not allow restoring someone else's snapshot.
	other := *s
	other.cfg.Dataset = "other"
	var reg Registration
	if err := readJSON(s.statePath("registration.json"), &reg); err != nil {
		t.Fatal(err)
	}
	reg.Dataset = "other"
	if err := writeJSON(s.statePath("registration.json"), reg); err != nil {
		t.Fatal(err)
	}
	if err := other.Run(ctx, []string{"restore", "--snapshot", id, "--target", filepath.Join(c.RestoreRoot, "wrong-dataset")}); err == nil {
		t.Fatal("cross-dataset restore allowed")
	}
	reg.Dataset = c.Dataset
	if err := writeJSON(s.statePath("registration.json"), reg); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	fail("backup") // Empty source does not become a successful recovery point.
	fail("snapshot")
	fail("prune") // Failed snapshots never trigger cleanup.
	run("status")
	if err := json.Unmarshal(output.Bytes(), &status); err != nil || status.LastSuccess.IsZero() || status.LastError == "" {
		t.Fatal("failure lost previous success status")
	}
	// Recovery remains possible even when the original source is missing.
	if err := os.RemoveAll(c.Source); err != nil {
		t.Fatal(err)
	}
	fail("backup")
	run("restore", "--snapshot", id, "--target", filepath.Join(c.RestoreRoot, "lost-disk"))
	if err := os.Mkdir(c.Source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("wrong-volume"), 0600); err != nil {
		t.Fatal(err)
	}
	fail("backup") // Missing identity, even though files exist.
	// A changed target identity must also block recovery.
	reg.SourceID = strings.Repeat("f", 64)
	if err := writeJSON(s.statePath("registration.json"), reg); err != nil {
		t.Fatal(err)
	}
	fail("restore", "--snapshot", id, "--target", filepath.Join(c.RestoreRoot, "wrong-repo"))
}

func TestServeCancellation(t *testing.T) {
	c := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := New(c, &bytes.Buffer{}, &bytes.Buffer{}).Run(ctx, []string{"serve"}); err != nil {
		t.Fatal(err)
	}
}
