package keepsync

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestAutomaticSetupReadOnlySourceAndRestart(t *testing.T) {
	c := testConfig(t)
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	file := filepath.Join(c.Source, "file.json")
	if err := os.WriteFile(file, []byte("latest"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.Source, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(c.Source, 0755) })
	if err := s.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal(err)
	}
	var reg Registration
	if err := readJSON(s.statePath("registration.json"), &reg); err != nil {
		t.Fatal(err)
	}
	if reg.IdentityMode != "state" || reg.SourceFileID == "" || !validSourceID(reg.SourceID) {
		t.Fatal("automatic identity not persisted")
	}
	if _, err := os.Stat(filepath.Join(c.Source, markerName)); !os.IsNotExist(err) {
		t.Fatal("automatic setup wrote to source")
	}
	restarted := New(c, io.Discard, io.Discard)
	if err := restarted.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal("restart failed:", err)
	}
	var after Registration
	if err := readJSON(s.statePath("registration.json"), &after); err != nil {
		t.Fatal(err)
	}
	if after.SourceID != reg.SourceID || after.SourceFileID != reg.SourceFileID {
		t.Fatal("restart replaced the source identity")
	}
	// A lost state volume must not silently claim a non-empty remote target.
	lost := c
	lost.StateDir = filepath.Join(filepath.Dir(c.StateDir), "new-state")
	if err := New(lost, io.Discard, io.Discard).Run(ctx, []string{"backup"}); err == nil {
		t.Fatal("lost state was silently rebound")
	}
	if b, err := os.ReadFile(filepath.Join(c.Repository, "Live", "file.json")); err != nil || string(b) != "latest" {
		t.Fatal("lost state changed remote data")
	}
	// Keep the original inode allocated so a replacement cannot reuse it.
	old := c.Source + "-old"
	if err := os.Rename(c.Source, old); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(old, 0755) })
	if err := os.Mkdir(c.Source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("wrong volume"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Run(ctx, []string{"backup"}); err == nil {
		t.Fatal("replacement volume accepted")
	}
	if b, err := os.ReadFile(filepath.Join(c.Repository, "Live", "file.json")); err != nil || string(b) != "latest" {
		t.Fatal("wrong volume overwrote latest backup")
	}
}

func TestInterruptedAutomaticSetupResumesSameIdentity(t *testing.T) {
	c := testConfig(t)
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(c.Source, "file"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(c.Source)
	if err != nil {
		t.Fatal(err)
	}
	_, fileID := fileMetadata(info)
	reg := Registration{Version: 2, Dataset: c.Dataset, Source: c.Source, Repository: c.Repository, SourceID: strings.Repeat("a", 64), IdentityMode: "state", SourceFileID: fileID}
	if err := writeJSON(s.statePath("setup-pending.json"), reg); err != nil {
		t.Fatal(err)
	}
	store, err := s.live(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Bind(ctx, reg.SourceID); err != nil {
		t.Fatal(err)
	}
	if err := New(c, io.Discard, io.Discard).Run(ctx, []string{"backup"}); err != nil {
		t.Fatal(err)
	}
	var recovered Registration
	if err := readJSON(s.statePath("registration.json"), &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.SourceID != reg.SourceID {
		t.Fatal("pending identity was replaced")
	}
	if _, err := os.Stat(s.statePath("setup-pending.json")); !os.IsNotExist(err) {
		t.Fatal("pending setup not finalized")
	}
}

func TestAutomaticS3SetupAllowsFolderPlaceholders(t *testing.T) {
	c, _, _ := fakeS3(t)
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(c.Source, "file.txt"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := s.live(ctx)
	if err != nil {
		t.Fatal(err)
	}
	remote := store.(*s3Store)
	_, err = remote.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("backups"), Key: aws.String("my-app/"), Body: strings.NewReader("")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal("empty S3 folder was rejected:", err)
	}
	if _, err := os.Stat(filepath.Join(c.Source, markerName)); !os.IsNotExist(err) {
		t.Fatal("S3 startup wrote a source marker")
	}
}

func TestComposeStyleStartupWaitsForFilesAndCreatesTar(t *testing.T) {
	c := testConfig(t)
	c.Method = "live"
	c.Interval = "1h"
	c.Debounce = "1s"
	c.MaxWait = "1s"
	c.Snapshots = SnapshotConfig{Enabled: true, Format: "tar", Interval: "30m", KeepLast: 7}
	c.LogFormat = "json"
	var logs safeLog
	s := New(c, &logs, io.Discard)
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
			t.Error("automatic service did not stop")
		}
	}()
	waitFor(t, 10*time.Second, func() bool { return logs.count("Waiting for source files") > 0 })
	if err := os.WriteFile(filepath.Join(c.Source, "new.json"), []byte("first file"), 0644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		var status Status
		return readJSON(s.statePath("status.json"), &status) == nil && !status.LastSuccess.IsZero() && status.LastSnapshot.IsZero() == false && status.SnapshotFormat == "tar" && status.SnapshotError == ""
	})
	if logs.count(`"level":"ERROR"`) != 0 {
		t.Fatal("ordinary first startup logged an operation error")
	}
	if _, err := os.Stat(filepath.Join(c.Source, markerName)); !os.IsNotExist(err) {
		t.Fatal("daemon wrote into source")
	}
	var reg Registration
	if err := readJSON(s.statePath("registration.json"), &reg); err != nil {
		t.Fatal(err)
	}
	var status Status
	if err := readJSON(s.statePath("status.json"), &status); err != nil {
		t.Fatal(err)
	}
	// The archive carries the identity even though the live source has no marker.
	f, err := s.downloadTar(context.Background(), reg, status.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	if err := readTar(context.Background(), f, reg, status.SnapshotID, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticEncryptedRepositoryInitialization(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		if os.Getenv("KEEPSYNC_REQUIRE_INTEGRATION") == "1" {
			t.Fatal(err)
		}
		t.Skip("restic not installed")
	}
	c := testConfig(t)
	c.Snapshots.Enabled = true
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(c.Source, "file"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, []string{"snapshot"}); err != nil {
		t.Fatal("automatic Restic init failed:", err)
	}
	var reg Registration
	if err := readJSON(s.statePath("registration.json"), &reg); err != nil {
		t.Fatal(err)
	}
	if reg.RepositoryID == "" {
		t.Fatal("encrypted repository not bound")
	}
	if _, err := os.Stat(filepath.Join(c.Source, markerName)); !os.IsNotExist(err) {
		t.Fatal("encrypted setup wrote to source")
	}
	if err := s.Run(ctx, []string{"check"}); err != nil {
		t.Fatal(err)
	}
}

func TestMissingSnapshotPasswordDoesNotBlockLive(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		if os.Getenv("KEEPSYNC_REQUIRE_INTEGRATION") == "1" {
			t.Fatal(err)
		}
		t.Skip("restic not installed")
	}
	c := testConfig(t)
	c.Snapshots.Enabled = true
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	if err := os.Remove(c.PasswordFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Source, "file"), []byte("protected"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal("Live depended on snapshot password:", err)
	}
	if err := s.Run(ctx, []string{"snapshot"}); err == nil {
		t.Fatal("encrypted snapshot accepted missing password")
	}
	var status Status
	if err := readJSON(s.statePath("status.json"), &status); err != nil {
		t.Fatal(err)
	}
	if status.LastSuccess.IsZero() || status.LastError != "" || status.SnapshotError == "" {
		t.Fatal("Live and snapshot statuses were not separated")
	}
}

func TestRestoredIdentityCanResumeAutomaticSource(t *testing.T) {
	c := testConfig(t)
	c.Snapshots.Enabled = true
	c.Snapshots.Format = "tar"
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(c.Source, "file"), []byte("recovery"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"backup", "snapshot"} {
		if err := s.Run(ctx, []string{command}); err != nil {
			t.Fatal(err)
		}
	}
	var status Status
	if err := readJSON(s.statePath("status.json"), &status); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(c.RestoreRoot, "point")
	if err := s.Run(ctx, []string{"restore", "--snapshot", status.SnapshotID, "--target", dest}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(c.Source, c.Source+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dest, "data"), c.Source); err != nil {
		t.Fatal(err)
	}
	if err := New(c, io.Discard, io.Discard).Run(ctx, []string{"backup"}); err != nil {
		t.Fatal("verified restored source could not resume:", err)
	}
}
