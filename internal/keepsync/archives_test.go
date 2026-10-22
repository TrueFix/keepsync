package keepsync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTarSnapshotsArePortableRetainedAndRecoverable(t *testing.T) {
	c, backend, fail := fakeS3(t)
	c.Snapshots = SnapshotConfig{Enabled: true, Format: "tar", Interval: "30m", KeepLast: 2}
	file := filepath.Join(c.Source, "FILEA.JSON")
	if err := os.WriteFile(file, []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		if err := s.Run(ctx, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	run("init")
	run("register-source")
	ids := []string{}
	for _, payload := range []string{"first", "second", "third"} {
		if err := os.WriteFile(file, []byte(payload), 0600); err != nil {
			t.Fatal(err)
		}
		run("backup")
		run("snapshot")
		var status Status
		if err := readJSON(s.statePath("status.json"), &status); err != nil {
			t.Fatal(err)
		}
		if status.SnapshotFormat != "tar" || !validArchiveID(status.SnapshotID) {
			t.Fatal("TAR success status missing")
		}
		ids = append(ids, status.SnapshotID)
	}
	reg, err := s.registration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store, err := s.archives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.ArchiveList(ctx, archiveScope(reg))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("retention kept %d archives, want 2", len(records))
	}
	for _, record := range records {
		if record.ID == ids[0] {
			t.Fatal("old archive was not expired")
		}
	}
	// A standard gzip/tar reader can open the S3 object independently.
	obj, err := backend.GetObject("backups", records[0].Key, nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := io.ReadAll(obj.Contents)
	obj.Contents.Close()
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "data/FILEA.JSON" {
			b, err := io.ReadAll(tr)
			if err != nil || string(b) != "third" {
				t.Fatal("archive did not contain readable source bytes")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("portable data entry missing")
	}
	if command, err := exec.LookPath("tar"); err == nil {
		path := filepath.Join(t.TempDir(), "download.tar.gz")
		if err := os.WriteFile(path, compressed, 0600); err != nil {
			t.Fatal(err)
		}
		manual := t.TempDir()
		if out, err := exec.Command(command, "-xzf", path, "-C", manual).CombinedOutput(); err != nil {
			t.Fatalf("standard tar extraction: %v: %s", err, out)
		}
		if b, err := os.ReadFile(filepath.Join(manual, "data", "FILEA.JSON")); err != nil || string(b) != "third" {
			t.Fatal("standard tar did not recover the file")
		}
	}
	// Failed uploads and failed archive creation never consume good history.
	fail.Store(true)
	if err := s.Run(ctx, []string{"snapshot"}); err == nil {
		t.Fatal("failed archive upload reported success")
	}
	fail.Store(false)
	if err := s.Run(ctx, []string{"prune"}); err == nil {
		t.Fatal("retention ran after a failed snapshot")
	}
	if records, err := store.ArchiveList(ctx, archiveScope(reg)); err != nil || len(records) != 2 {
		t.Fatal("upload failure removed history")
	}
	bad := filepath.Join(c.Source, "outside-link")
	if err := os.Symlink("/etc/passwd", bad); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, []string{"snapshot"}); err == nil {
		t.Fatal("unsafe source symlink was archived")
	}
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	run("check")
	// Archive recovery does not require the original source data.
	if err := os.RemoveAll(c.Source); err != nil {
		t.Fatal(err)
	}
	s.cfg.Snapshots.Enabled = false
	dest := filepath.Join(c.RestoreRoot, "recovered")
	if err := os.Mkdir(dest, 0700); err != nil {
		t.Fatal(err)
	}
	run("restore", "--snapshot", ids[1], "--target", dest)
	if b, err := os.ReadFile(filepath.Join(dest, "data", "FILEA.JSON")); err != nil || string(b) != "second" {
		t.Fatal("old TAR recovery failed")
	}
	if b, err := os.ReadFile(filepath.Join(dest, "data", markerName)); err != nil || string(b) != reg.SourceID {
		t.Fatal("source identity was not recoverable")
	}
	if err := s.Run(ctx, []string{"restore", "--snapshot", ids[1], "--target", dest}); err == nil {
		t.Fatal("restore overwrote an existing recovery")
	}
}

func TestTarMultipartArchiveUpload(t *testing.T) {
	c, _, _ := fakeS3(t)
	c.Snapshots.Format = "tar"
	c.Snapshots.Enabled = true
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	payload := make([]byte, 9<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Source, "large.bin"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"register-source", "snapshot", "check"} {
		if err := s.Run(ctx, []string{command}); err != nil {
			t.Fatalf("%s: %v", command, err)
		}
	}
	var status Status
	if err := readJSON(s.statePath("status.json"), &status); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(c.RestoreRoot, "large")
	if err := s.Run(ctx, []string{"restore", "--snapshot", status.SnapshotID, "--target", dest}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "data", "large.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("multipart archive payload differs")
	}
}

type tarEntry struct {
	name, link, body string
	kind             byte
}

func archiveFixture(t *testing.T, reg Registration, id string, entries []tarEntry) []byte {
	t.Helper()
	created, err := archiveTime(id)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(archiveManifest{Version: 1, Format: "tar.gz", ID: id, Dataset: reg.Dataset, SourceID: reg.SourceID, CreatedAt: created})
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "keepsync.json", Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(metadata))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(metadata); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		h := &tar.Header{Name: entry.name, Typeflag: entry.kind, Mode: 0600, Linkname: entry.link}
		if entry.kind == tar.TypeReg {
			h.Size = int64(len(entry.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if entry.body != "" {
			if _, err := tw.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestTarRestoreRejectsUnsafeOrCorruptArchivesAtomically(t *testing.T) {
	for _, tt := range []struct {
		name    string
		entries []tarEntry
		corrupt bool
	}{
		{"traversal", []tarEntry{{name: "data/../../outside", body: "bad", kind: tar.TypeReg}}, false},
		{"absolute", []tarEntry{{name: "/outside", body: "bad", kind: tar.TypeReg}}, false},
		{"escaping link", []tarEntry{{name: "data/link", link: "../../outside", kind: tar.TypeSymlink}}, false},
		{"link traversal", []tarEntry{{name: "data/link", link: "inside", kind: tar.TypeSymlink}, {name: "data/link/file", body: "bad", kind: tar.TypeReg}}, false},
		{"hard link", []tarEntry{{name: "data/link", link: "../../outside", kind: tar.TypeLink}}, false},
		{"duplicate file", []tarEntry{{name: "data/file", body: "a", kind: tar.TypeReg}, {name: "data/file", body: "b", kind: tar.TypeReg}}, false},
		{"gzip checksum", []tarEntry{{name: "data/file", body: "content", kind: tar.TypeReg}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig(t)
			c.Snapshots.Enabled = true
			c.Snapshots.Format = "tar"
			s := New(c, io.Discard, io.Discard)
			ctx := context.Background()
			if err := os.WriteFile(filepath.Join(c.Source, "original"), []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.Run(ctx, []string{"register-source"}); err != nil {
				t.Fatal(err)
			}
			reg, err := s.registration(ctx)
			if err != nil {
				t.Fatal(err)
			}
			id := time.Now().UTC().Format(archiveClock) + "-0123456789abcdef.tar.gz"
			data := archiveFixture(t, reg, id, tt.entries)
			if tt.corrupt {
				data[len(data)-8] ^= 0xff
			}
			store := s.store.(*localStore)
			dir, err := store.archiveDir(archiveScope(reg))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, id), data, 0600); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(c.RestoreRoot, "bad")
			if err := s.Run(ctx, []string{"restore", "--snapshot", id, "--target", dest}); err == nil {
				t.Fatal("unsafe archive was restored")
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("failed restore exposed partial destination")
			}
			if b, err := os.ReadFile(filepath.Join(c.Source, "original")); err != nil || string(b) != "untouched" {
				t.Fatal("restore touched the source")
			}
			entries, err := os.ReadDir(c.RestoreRoot)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".keepsync-restore-") {
					t.Fatal("failed restore staging directory leaked")
				}
			}
		})
	}
}

func TestLegacySnapshotStatusSchedulesTarImmediately(t *testing.T) {
	c := testConfig(t)
	c.Snapshots.Enabled = true
	s := New(c, io.Discard, io.Discard)
	status := Status{LastSnapshot: time.Now().UTC(), SnapshotFormat: "restic"}
	if err := writeJSON(s.statePath("status.json"), status); err != nil {
		t.Fatal(err)
	}
	if !s.snapshotDue(time.Now()) || s.snapshotDeadline(time.Now()).After(time.Now().Add(time.Second)) {
		t.Fatal("legacy snapshot status did not schedule a TAR recovery point")
	}
}
