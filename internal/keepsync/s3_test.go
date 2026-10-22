package keepsync

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

func fakeS3(t *testing.T) (Config, *s3mem.Backend, *atomic.Bool) {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("backups"); err != nil {
		t.Fatal(err)
	}
	fake := gofakes3.New(backend).Server()
	fail := new(atomic.Bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() && r.Method == http.MethodPut && (strings.Contains(r.URL.Path, "/Live/file.txt") || strings.Contains(r.URL.Path, "/Snapshots/tar/") || strings.Contains(r.URL.Path, "/snapshots/")) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(503)
			_, _ = io.WriteString(w, "<Error><Code>ServiceUnavailable</Code></Error>")
			return
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_DEFAULT_REGION", "us-east-1")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	c := testConfig(t)
	c.Repository = "s3://backups/my-app"
	c.S3Endpoint = server.URL
	return c, backend, fail
}

func TestS3SnapshotsRetentionAndRecovery(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		if os.Getenv("KEEPSYNC_REQUIRE_INTEGRATION") == "1" {
			t.Fatal(err)
		}
		t.Skip("restic not installed")
	}
	c, backend, _ := fakeS3(t)
	c.Snapshots.Enabled = true
	c.Snapshots.KeepLast = 2
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		if err := s.Run(ctx, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	file := filepath.Join(c.Source, "file.txt")
	if err := os.WriteFile(file, []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}
	run("init")
	run("register-source")
	ids := []string{}
	for _, text := range []string{"first", "second", "third"} {
		if err := os.WriteFile(file, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		run("backup")
		run("snapshot")
		var status Status
		if err := readJSON(s.statePath("status.json"), &status); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, status.SnapshotID)
		if text == "second" {
			if err := s.restic(ctx, io.Discard, "backup", "--tag", s.tag()+":pending", "--host", c.Dataset, "--", c.Source); err != nil {
				t.Fatal(err)
			}
		}
	}
	var out bytes.Buffer
	if err := s.restic(ctx, &out, "snapshots", "--json", "--tag", s.tag()); err != nil {
		t.Fatal(err)
	}
	var snapshots []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out.Bytes(), &snapshots); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("expected exactly 2 snapshots, got %d", len(snapshots))
	}
	for _, snapshot := range snapshots {
		if snapshot.ID == ids[0] {
			t.Fatal("old snapshot was not expired")
		}
	}
	obj, err := backend.GetObject("backups", "my-app/Live/file.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(obj.Contents)
	obj.Contents.Close()
	if err != nil || string(b) != "third" {
		t.Fatal("retention changed the Live file")
	}
	// Disabling new snapshots must not prevent restoring existing history.
	s.cfg.Snapshots.Enabled = false
	target := filepath.Join(c.RestoreRoot, "old")
	run("restore", "--snapshot", ids[1][:9], "--target", target)
	recovered := filepath.Join(target, strings.TrimPrefix(file, string(filepath.Separator)))
	if b, err := os.ReadFile(recovered); err != nil || string(b) != "second" {
		t.Fatalf("old snapshot recovery: %s, %v", b, err)
	}
	if err := s.Run(ctx, []string{"snapshot"}); err == nil {
		t.Fatal("snapshot created while history is disabled")
	}
}

func TestMissingIdentityMarkerSelfHeals(t *testing.T) {
	c, backend, _ := fakeS3(t)
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	file := filepath.Join(c.Source, "file.txt")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, []string{"register-source"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal(err)
	}
	var reg Registration
	if err := readJSON(s.statePath("registration.json"), &reg); err != nil {
		t.Fatal(err)
	}
	// Simulate an operator accidentally deleting the identity marker on S3.
	if _, err := backend.DeleteObject("backups", "my-app/Live/"+markerName); err != nil {
		t.Fatal(err)
	}
	fresh := New(c, io.Discard, io.Discard)
	if err := fresh.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal("backup must self-heal a missing identity marker local state already trusts:", err)
	}
	obj, err := backend.GetObject("backups", "my-app/Live/"+markerName, nil)
	if err != nil {
		t.Fatal("marker was not recreated:", err)
	}
	b, err := io.ReadAll(obj.Contents)
	obj.Contents.Close()
	if err != nil || string(b) != reg.SourceID {
		t.Fatal("recreated marker does not match the trusted local identity")
	}
	// A marker claimed by a different source must never be overwritten.
	if _, err := backend.DeleteObject("backups", "my-app/Live/"+markerName); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.PutObject("backups", "my-app/Live/"+markerName, nil, strings.NewReader(strings.Repeat("b", 64)), 64, nil); err != nil {
		t.Fatal(err)
	}
	if err := New(c, io.Discard, io.Discard).Run(ctx, []string{"backup"}); err == nil {
		t.Fatal("backup must refuse when the marker belongs to a different source")
	}
}

func TestReclaimForceClaimsIdentitylessTarget(t *testing.T) {
	c, backend, _ := fakeS3(t)
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	file := filepath.Join(c.Source, "file.txt")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	// Automatic state-backed setup, as used by the default read-only Compose
	// deployment: no marker file is ever written onto the source.
	if err := s.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal(err)
	}
	// Losing both the state volume and the remote marker leaves nothing to
	// verify against; reclaim must refuse without an explicit --force.
	if err := os.Remove(s.statePath("registration.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.DeleteObject("backups", "my-app/Live/"+markerName); err != nil {
		t.Fatal(err)
	}
	lost := New(c, io.Discard, io.Discard)
	if err := lost.Run(ctx, []string{"reclaim"}); err == nil {
		t.Fatal("reclaim without --force must not claim an identityless target")
	}
	if err := lost.Run(ctx, []string{"reclaim", "--force"}); err != nil {
		t.Fatal("reclaim --force failed:", err)
	}
	var reg Registration
	if err := readJSON(lost.statePath("registration.json"), &reg); err != nil {
		t.Fatal(err)
	}
	if !validSourceID(reg.SourceID) {
		t.Fatal("forced reclaim produced an invalid source identity")
	}
	obj, err := backend.GetObject("backups", "my-app/Live/"+markerName, nil)
	if err != nil {
		t.Fatal("marker was not created:", err)
	}
	b, err := io.ReadAll(obj.Contents)
	obj.Contents.Close()
	if err != nil || string(b) != reg.SourceID {
		t.Fatal("remote marker does not match the claimed local identity")
	}
	if err := lost.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal("backup after forced reclaim failed:", err)
	}
	// Once claimed, a second forced reclaim from fresh state must agree with
	// the existing marker instead of minting a conflicting new identity.
	if err := os.Remove(lost.statePath("registration.json")); err != nil {
		t.Fatal(err)
	}
	again := New(c, io.Discard, io.Discard)
	if err := again.Run(ctx, []string{"reclaim", "--force"}); err != nil {
		t.Fatal("reclaim --force on an already-claimed target failed:", err)
	}
	var reclaimedAgain Registration
	if err := readJSON(again.statePath("registration.json"), &reclaimedAgain); err != nil {
		t.Fatal(err)
	}
	if reclaimedAgain.SourceID != reg.SourceID {
		t.Fatal("forced reclaim on an already-claimed target changed the identity")
	}
}

func TestS3LiveUploadsRetryAndRepair(t *testing.T) {
	c, backend, fail := fakeS3(t)
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	file := filepath.Join(c.Source, "file.txt")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, []string{"register-source"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		obj, err := backend.GetObject("backups", "my-app/Live/file.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer obj.Contents.Close()
		b, err := io.ReadAll(obj.Contents)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if read() != "original" {
		t.Fatal("S3 payload differs from source")
	}
	if err := os.WriteFile(file, []byte("modified"), 0600); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if err := s.Run(ctx, []string{"backup"}); err == nil {
		t.Fatal("failed upload reported success")
	}
	if read() != "original" {
		t.Fatal("failed upload replaced last good Live object")
	}
	var status Status
	if err := readJSON(s.statePath("status.json"), &status); err != nil || status.LastSuccess.IsZero() || status.LastError == "" {
		t.Fatal("failure status lost last success")
	}
	fail.Store(false)
	if err := s.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal(err)
	}
	if read() != "modified" {
		t.Fatal("retry did not upload latest content")
	}
	store, err := s.live(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "file.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal(err)
	}
	if read() != "modified" {
		t.Fatal("missing object was not repaired")
	}
}

func TestTargetParsingAndIsolation(t *testing.T) {
	for _, raw := range []string{"", "https://example.com/bucket", "s3://bucket/a/../b", "s3://user:password@bucket/path", "s3://bucket/path?secret=value"} {
		if _, err := parseTarget(raw, ""); err == nil {
			t.Fatalf("accepted unsafe target %q", raw)
		}
	}
	target, err := parseTarget("s3://bucket/my-app", "")
	if err != nil {
		t.Fatal(err)
	}
	if target.snapshots() != "s3:https://s3.amazonaws.com/bucket/my-app/Snapshots" {
		t.Fatal(target.snapshots())
	}
	if _, err := parseTarget("s3://bucket/prefix", "https://user:secret@example.com"); err == nil {
		t.Fatal("accepted endpoint credentials")
	}
	for _, rel := range []string{"../Snapshots/config", "/absolute", "a/../../b", markerName, "a\\b"} {
		if validRelativeFile(rel) {
			t.Fatalf("accepted unsafe index path %q", rel)
		}
	}
}
