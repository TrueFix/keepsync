package keepsync

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type archiveRecord struct {
	ID          string    `json:"id"`
	Key         string    `json:"key"`
	MetadataKey string    `json:"metadata_key,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	Size        int64     `json:"size_bytes"`
	Format      string    `json:"format"`
}

// The JSON sidecar is the commit record. It is published only after the complete
// immutable TAR object. The embedded manifest remains sufficient for PC recovery.
type archiveMetadata struct {
	archiveManifest
	ArchiveKey string `json:"archive_key"`
	SourcePath string `json:"source_path"`
	Size       int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"`
}

func archiveScope(reg Registration) string { return reg.Dataset + "/" + reg.SourceID }
func archiveKey(id string) string          { return path.Join("snapshots", id) }
func archiveMetadataKey(id string) string {
	return path.Join("snapshots", strings.TrimSuffix(id, ".tar.gz")+".json")
}
func legacyArchiveKey(scope, id string) string { return path.Join("Snapshots", "tar", scope, id) }
func validArchiveScope(scope string) bool {
	parts := strings.Split(scope, "/")
	return len(parts) == 2 && validSourceID(parts[1]) && parts[0] != "" && strings.Trim(parts[0], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") == ""
}
func (m archiveMetadata) scope() string { return m.Dataset + "/" + m.SourceID }
func (m archiveMetadata) validate(id string) error {
	created, err := archiveTime(id)
	if err != nil {
		return err
	}
	if m.Version != 1 || m.Format != "tar.gz" || m.ID != id || !m.CreatedAt.Equal(created) || !validArchiveScope(m.scope()) || m.ArchiveKey != archiveKey(id) || m.Size < 1 || !validSourceID(m.SHA256) || !strings.HasPrefix(m.SourcePath, "/") {
		return errors.New("invalid snapshot metadata")
	}
	return nil
}
func sortArchives(records []archiveRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].ID > records[j].ID
		}
		return records[i].CreatedAt.After(records[j].CreatedAt)
	})
}

type archiveStore interface {
	ArchivePut(context.Context, archiveMetadata, *os.File) error
	ArchiveList(context.Context, string) ([]archiveRecord, error)
	ArchiveOpen(context.Context, string, string) (io.ReadCloser, string, error)
	ArchiveDelete(context.Context, string, string) error
}

type archiveObjectInfo struct {
	size int64
	hash string
}
type archiveObjects interface {
	put(context.Context, string, io.Reader, int64, string, string) error
	open(context.Context, string) (io.ReadCloser, archiveObjectInfo, error)
	stat(context.Context, string) (archiveObjectInfo, error)
	walk(context.Context, string, func(string, archiveObjectInfo) error) error
	remove(context.Context, string) error
	key(string) string
}
type archiveRepository struct{ objects archiveObjects }

func (s *Service) archives(ctx context.Context) (archiveStore, error) {
	store, err := s.live(ctx)
	if err != nil {
		return nil, err
	}
	switch target := store.(type) {
	case *s3Store:
		return &archiveRepository{&s3ArchiveObjects{target}}, nil
	case *localStore:
		repo, err := resolved(filepath.Dir(target.root))
		if err != nil {
			return nil, err
		}
		return &archiveRepository{&localArchiveObjects{repo}}, nil
	default:
		return nil, errors.New("target does not support portable archives")
	}
}

func (r *archiveRepository) metadata(ctx context.Context, id string) (archiveMetadata, bool, error) {
	var metadata archiveMetadata
	body, _, err := r.objects.open(ctx, archiveMetadataKey(id))
	if archiveMissing(err) {
		return metadata, false, nil
	}
	if err != nil {
		return metadata, false, err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 64<<10+1))
	if err != nil {
		return metadata, false, err
	}
	if len(raw) > 64<<10 {
		return metadata, false, errors.New("snapshot metadata exceeds 64 KiB")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&metadata); err != nil {
		return metadata, false, fmt.Errorf("invalid snapshot metadata: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return metadata, false, errors.New("snapshot metadata must contain one JSON object")
	}
	if err := metadata.validate(id); err != nil {
		return metadata, false, err
	}
	return metadata, true, nil
}

func (r *archiveRepository) ArchivePut(ctx context.Context, metadata archiveMetadata, f *os.File) error {
	if err := metadata.validate(metadata.ID); err != nil {
		return err
	}
	if err := r.objects.put(ctx, archiveKey(metadata.ID), f, metadata.Size, metadata.SHA256, "application/gzip"); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := r.objects.put(ctx, archiveMetadataKey(metadata.ID), bytes.NewReader(raw), int64(len(raw)), "", "application/json"); err != nil {
		return fmt.Errorf("archive uploaded but metadata publication failed: %w", err)
	}
	return nil
}

func (r *archiveRepository) ArchiveList(ctx context.Context, scope string) ([]archiveRecord, error) {
	if !validArchiveScope(scope) {
		return nil, errors.New("invalid archive source scope")
	}
	records := []archiveRecord{}
	seen := make(map[string]bool)
	err := r.objects.walk(ctx, "snapshots/", func(key string, _ archiveObjectInfo) error {
		name := strings.TrimPrefix(key, "snapshots/")
		if strings.Contains(name, "/") || !strings.HasSuffix(name, ".json") {
			return nil
		}
		id := strings.TrimSuffix(name, ".json") + ".tar.gz"
		if !validArchiveID(id) {
			return nil
		}
		metadata, exists, err := r.metadata(ctx, id)
		if err != nil {
			return err
		}
		if !exists || metadata.scope() != scope {
			return nil
		}
		info, err := r.objects.stat(ctx, archiveKey(id))
		if archiveMissing(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.size != metadata.Size || (info.hash != "" && info.hash != metadata.SHA256) {
			return fmt.Errorf("snapshot %s does not match its metadata", id)
		}
		records = append(records, archiveRecord{ID: id, Key: r.objects.key(archiveKey(id)), MetadataKey: r.objects.key(archiveMetadataKey(id)), CreatedAt: metadata.CreatedAt, Size: metadata.Size, Format: "tar"})
		seen[id] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Existing backups remain in place; only new snapshots use the flat layout.
	prefix := legacyArchiveKey(scope, "") + "/"
	err = r.objects.walk(ctx, prefix, func(key string, info archiveObjectInfo) error {
		id := strings.TrimPrefix(key, prefix)
		if !validArchiveID(id) || seen[id] {
			return nil
		}
		created, err := archiveTime(id)
		if err != nil {
			return nil
		}
		records = append(records, archiveRecord{ID: id, Key: r.objects.key(key), CreatedAt: created, Size: info.size, Format: "tar"})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortArchives(records)
	return records, nil
}

func (r *archiveRepository) ArchiveOpen(ctx context.Context, scope, id string) (io.ReadCloser, string, error) {
	if !validArchiveID(id) || !validArchiveScope(scope) {
		return nil, "", errors.New("invalid archive ID or source scope")
	}
	metadata, exists, err := r.metadata(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if exists && metadata.scope() == scope {
		body, info, err := r.objects.open(ctx, archiveKey(id))
		if err != nil {
			return nil, "", err
		}
		if info.size != metadata.Size || (info.hash != "" && info.hash != metadata.SHA256) {
			body.Close()
			return nil, "", errors.New("archive differs from its metadata")
		}
		return body, metadata.SHA256, nil
	}
	body, info, err := r.objects.open(ctx, legacyArchiveKey(scope, id))
	return body, info.hash, err
}

func (r *archiveRepository) ArchiveDelete(ctx context.Context, scope, id string) error {
	if !validArchiveID(id) || !validArchiveScope(scope) {
		return errors.New("invalid archive ID or source scope")
	}
	metadata, exists, err := r.metadata(ctx, id)
	if err != nil {
		return err
	}
	if exists && metadata.scope() == scope {
		if err := r.objects.remove(ctx, archiveKey(id)); err != nil {
			return err
		}
		if err := r.objects.remove(ctx, archiveMetadataKey(id)); err != nil {
			return err
		}
	}
	// Expire an older copy too if the same logical recovery point was copied to
	// the flat layout. Never touch a flat pair owned by a different source.
	return r.objects.remove(ctx, legacyArchiveKey(scope, id))
}

func archiveMissing(err error) bool { return errors.Is(err, os.ErrNotExist) || missingObject(err) }

type s3ArchiveObjects struct{ store *s3Store }

func (o *s3ArchiveObjects) key(key string) string { return path.Join(o.store.target.prefix, key) }
func (o *s3ArchiveObjects) put(ctx context.Context, key string, body io.Reader, size int64, hash, contentType string) error {
	var metadata map[string]string
	if hash != "" {
		metadata = map[string]string{"keepsync-sha256": hash, "keepsync-format": "tar"}
	}
	_, err := o.store.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{Bucket: aws.String(o.store.target.bucket), Key: aws.String(o.key(key)), Body: body, ContentLength: aws.Int64(size), ContentType: aws.String(contentType), IfNoneMatch: aws.String("*"), Metadata: metadata})
	return err
}
func (o *s3ArchiveObjects) open(ctx context.Context, key string) (io.ReadCloser, archiveObjectInfo, error) {
	r, err := o.store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(o.store.target.bucket), Key: aws.String(o.key(key))})
	if err != nil {
		return nil, archiveObjectInfo{}, err
	}
	return r.Body, archiveObjectInfo{size: aws.ToInt64(r.ContentLength), hash: r.Metadata["keepsync-sha256"]}, nil
}
func (o *s3ArchiveObjects) stat(ctx context.Context, key string) (archiveObjectInfo, error) {
	r, err := o.store.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(o.store.target.bucket), Key: aws.String(o.key(key))})
	if err != nil {
		return archiveObjectInfo{}, err
	}
	return archiveObjectInfo{size: aws.ToInt64(r.ContentLength), hash: r.Metadata["keepsync-sha256"]}, nil
}
func (o *s3ArchiveObjects) walk(ctx context.Context, prefix string, visit func(string, archiveObjectInfo) error) error {
	fullPrefix := o.key(strings.TrimSuffix(prefix, "/")) + "/"
	pages := s3.NewListObjectsV2Paginator(o.store.client, &s3.ListObjectsV2Input{Bucket: aws.String(o.store.target.bucket), Prefix: aws.String(fullPrefix), Delimiter: aws.String("/")})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			rel := strings.TrimPrefix(key, fullPrefix)
			if rel == "" || strings.Contains(rel, "/") {
				continue
			}
			if err := visit(prefix+rel, archiveObjectInfo{size: aws.ToInt64(object.Size)}); err != nil {
				return err
			}
		}
	}
	return nil
}
func (o *s3ArchiveObjects) remove(ctx context.Context, key string) error {
	_, err := o.store.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(o.store.target.bucket), Key: aws.String(o.key(key))})
	return err
}

type localArchiveObjects struct{ repo string }

func (o *localArchiveObjects) key(key string) string   { return key }
func (o *localArchiveObjects) root() (*os.Root, error) { return os.OpenRoot(o.repo) }
func (o *localArchiveObjects) put(ctx context.Context, key string, body io.Reader, size int64, hash, contentType string) error {
	root, err := o.root()
	if err != nil {
		return err
	}
	defer root.Close()
	dir := filepath.Dir(filepath.FromSlash(key))
	if err := root.MkdirAll(dir, 0700); err != nil {
		return err
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	stage := filepath.Join(dir, ".pending-"+hex.EncodeToString(random[:]))
	f, err := root.OpenFile(stage, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(stage)
	n, err := io.Copy(f, &contextReader{ctx, body})
	if err == nil && n != size {
		err = io.ErrUnexpectedEOF
	}
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := root.Link(stage, filepath.FromSlash(key)); err != nil {
		return err
	}
	parent, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
func (o *localArchiveObjects) open(ctx context.Context, key string) (io.ReadCloser, archiveObjectInfo, error) {
	root, err := o.root()
	if err != nil {
		return nil, archiveObjectInfo{}, err
	}
	defer root.Close()
	info, err := root.Lstat(filepath.FromSlash(key))
	if err != nil {
		return nil, archiveObjectInfo{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, archiveObjectInfo{}, errors.New("snapshot object is not a regular file")
	}
	f, err := root.Open(filepath.FromSlash(key))
	return f, archiveObjectInfo{size: info.Size()}, err
}
func (o *localArchiveObjects) stat(ctx context.Context, key string) (archiveObjectInfo, error) {
	body, info, err := o.open(ctx, key)
	if err != nil {
		return info, err
	}
	return info, body.Close()
}
func (o *localArchiveObjects) walk(ctx context.Context, prefix string, visit func(string, archiveObjectInfo) error) error {
	root, err := o.root()
	if archiveMissing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	dir, err := root.Open(filepath.FromSlash(strings.TrimSuffix(prefix, "/")))
	if archiveMissing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !entry.Type().IsRegular() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if err := visit(prefix+entry.Name(), archiveObjectInfo{size: info.Size()}); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
func (o *localArchiveObjects) remove(ctx context.Context, key string) error {
	root, err := o.root()
	if archiveMissing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	err = root.Remove(filepath.FromSlash(key))
	if archiveMissing(err) {
		return nil
	}
	return err
}

// Retained only for compatibility fixtures and access to the previous layout.
func (s *localStore) archiveDir(scope string) (string, error) {
	if !validArchiveScope(scope) {
		return "", errors.New("invalid archive source scope")
	}
	repo, err := resolved(filepath.Dir(s.root))
	if err != nil {
		return "", err
	}
	base := filepath.Join(repo, "Snapshots", "tar")
	dir, err := resolved(filepath.Join(base, filepath.FromSlash(scope)))
	if err != nil {
		return "", err
	}
	if !within(base, dir) || base == dir {
		return "", errors.New("archive scope escapes target")
	}
	return dir, nil
}
