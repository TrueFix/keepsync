package keepsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFlatArchiveLayoutAndMetadata(t *testing.T) {
	for _, kind := range []string{"local", "s3"} {
		t.Run(kind, func(t *testing.T) {
			var c Config
			if kind == "s3" {
				c, _, _ = fakeS3(t)
			} else {
				c = testConfig(t)
			}
			c.Snapshots.Enabled = true
			c.Snapshots.Format = "tar"
			c.Snapshots.KeepLast = 2
			s := New(c, io.Discard, io.Discard)
			ctx := context.Background()
			file := filepath.Join(c.Source, "file.json")
			if err := os.WriteFile(file, []byte("first"), 0600); err != nil {
				t.Fatal(err)
			}
			ids := []string{}
			for _, body := range []string{"first", "second", "third"} {
				if err := os.WriteFile(file, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				if err := s.Run(ctx, []string{"snapshot"}); err != nil {
					t.Fatal(err)
				}
				var status Status
				if err := readJSON(s.statePath("status.json"), &status); err != nil {
					t.Fatal(err)
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
			repo := store.(*archiveRepository)
			records, err := store.ArchiveList(ctx, archiveScope(reg))
			if err != nil || len(records) != 2 {
				t.Fatalf("list: %v, %v", records, err)
			}
			for _, record := range records {
				if !strings.HasSuffix(record.Key, "/snapshots/"+record.ID) && record.Key != "snapshots/"+record.ID {
					t.Fatal("archive was not stored flat:", record.Key)
				}
				if record.MetadataKey == "" {
					t.Fatal("missing sidecar key")
				}
				metadata, found, err := repo.metadata(ctx, record.ID)
				if err != nil || !found {
					t.Fatal("sidecar not readable:", err)
				}
				if metadata.Dataset != reg.Dataset || metadata.SourceID != reg.SourceID || metadata.SourcePath != c.Source || metadata.Size != record.Size || metadata.SHA256 == "" {
					t.Fatal("sidecar source or integrity information missing")
				}
			}
			for _, key := range []string{archiveKey(ids[0]), archiveMetadataKey(ids[0])} {
				if _, err := repo.objects.stat(ctx, key); !archiveMissing(err) {
					t.Fatal("retention left part of an expired pair:", key, err)
				}
			}
			if kind == "local" {
				entries, err := os.ReadDir(filepath.Join(c.Repository, "snapshots"))
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 4 {
					t.Fatalf("expected two pairs, got %d objects", len(entries))
				}
				for _, entry := range entries {
					if entry.IsDir() {
						t.Fatal("flat snapshots contain a subdirectory")
					}
				}
			}
		})
	}
}

type failSidecarObjects struct{ archiveObjects }

func (o failSidecarObjects) put(ctx context.Context, key string, body io.Reader, size int64, hash, contentType string) error {
	if strings.HasSuffix(key, ".json") {
		return errors.New("metadata unavailable")
	}
	return o.archiveObjects.put(ctx, key, body, size, hash, contentType)
}

func TestArchiveNotCommittedWithoutMetadata(t *testing.T) {
	c := testConfig(t)
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(c.Source, "file"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal(err)
	}
	reg, err := s.registration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store, err := s.archives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base := store.(*archiveRepository)
	id := time.Now().UTC().Format(archiveClock) + "-0123456789abcdef.tar.gz"
	created, err := archiveTime(id)
	if err != nil {
		t.Fatal(err)
	}
	payload := archiveFixture(t, reg, id, []tarEntry{{name: "data/file", body: "data", kind: '0'}})
	f, err := os.CreateTemp(c.StateDir, "fixture-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	if _, err := f.Write(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	metadata := archiveMetadata{archiveManifest: archiveManifest{Version: 1, Format: "tar.gz", ID: id, Dataset: reg.Dataset, SourceID: reg.SourceID, CreatedAt: created}, ArchiveKey: archiveKey(id), SourcePath: c.Source, Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])}
	faulty := &archiveRepository{failSidecarObjects{base.objects}}
	if err := faulty.ArchivePut(ctx, metadata, f); err == nil {
		t.Fatal("metadata publication failure was ignored")
	}
	if _, err := base.objects.stat(ctx, archiveKey(id)); err != nil {
		t.Fatal("test did not upload the TAR first")
	}
	if records, err := base.ArchiveList(ctx, archiveScope(reg)); err != nil || len(records) != 0 {
		t.Fatal("uncommitted archive was exposed as a recovery point")
	}
	if body, _, err := base.ArchiveOpen(ctx, archiveScope(reg), id); err == nil {
		body.Close()
		t.Fatal("uncommitted archive opened as confirmed")
	}
}

func TestFlatArchiveSourceIsolation(t *testing.T) {
	c := testConfig(t)
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(c.Source, "file"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal(err)
	}
	reg, err := s.registration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store, err := s.archives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo := store.(*archiveRepository)
	foreign := reg
	foreign.Dataset = "another-source"
	foreign.SourceID = strings.Repeat("b", 64)
	id := time.Now().UTC().Format(archiveClock) + "-fedcba9876543210.tar.gz"
	created, _ := archiveTime(id)
	payload := archiveFixture(t, foreign, id, []tarEntry{{name: "data/file", body: "foreign", kind: '0'}})
	sum := sha256.Sum256(payload)
	metadata := archiveMetadata{archiveManifest: archiveManifest{Version: 1, Format: "tar.gz", ID: id, Dataset: foreign.Dataset, SourceID: foreign.SourceID, CreatedAt: created}, ArchiveKey: archiveKey(id), SourcePath: "/data", Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])}
	if err := repo.objects.put(ctx, archiveKey(id), bytes.NewReader(payload), int64(len(payload)), metadata.SHA256, "application/gzip"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.objects.put(ctx, archiveMetadataKey(id), bytes.NewReader(raw), int64(len(raw)), "", "application/json"); err != nil {
		t.Fatal(err)
	}
	if records, err := store.ArchiveList(ctx, archiveScope(reg)); err != nil || len(records) != 0 {
		t.Fatal("another source's metadata was listed")
	}
	if err := store.ArchiveDelete(ctx, archiveScope(reg), id); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{archiveKey(id), archiveMetadataKey(id)} {
		if _, err := repo.objects.stat(ctx, key); err != nil {
			t.Fatal("another source's pair was deleted")
		}
	}
}

func TestLegacyArchivesRemainRecoverableAndRetained(t *testing.T) {
	c := testConfig(t)
	c.Snapshots.Enabled = true
	c.Snapshots.Format = "tar"
	c.Snapshots.KeepLast = 2
	s := New(c, io.Discard, io.Discard)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(c.Source, "file"), []byte("current"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, []string{"backup"}); err != nil {
		t.Fatal(err)
	}
	reg, err := s.registration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := time.Now().UTC().Add(-time.Hour).Format(archiveClock) + "-0123456789abcdef.tar.gz"
	payload := archiveFixture(t, reg, id, []tarEntry{{name: "data/file", body: "legacy", kind: '0'}})
	dir, err := s.store.(*localStore).archiveDir(archiveScope(reg))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id), payload, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(ctx, []string{"snapshot"}); err != nil {
		t.Fatal(err)
	}
	store, err := s.archives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if records, err := store.ArchiveList(ctx, archiveScope(reg)); err != nil || len(records) != 2 {
		t.Fatal("legacy and flat history did not merge")
	}
	dest := filepath.Join(c.RestoreRoot, "legacy")
	if err := s.Run(ctx, []string{"restore", "--snapshot", id, "--target", dest}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "data", "file")); err != nil || string(b) != "legacy" {
		t.Fatal("legacy recovery failed")
	}
	if err := s.Run(ctx, []string{"snapshot"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, id)); !os.IsNotExist(err) {
		t.Fatal("legacy point was not expired by combined retention")
	}
	if records, err := store.ArchiveList(ctx, archiveScope(reg)); err != nil || len(records) != 2 {
		t.Fatal("combined retention count incorrect")
	}
}
