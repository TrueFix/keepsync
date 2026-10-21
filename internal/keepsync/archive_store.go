package keepsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type archiveRecord struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
	Size      int64     `json:"size_bytes"`
	Format    string    `json:"format"`
}

type archiveStore interface {
	ArchivePut(context.Context, string, string, *os.File, int64, string) error
	ArchiveList(context.Context, string) ([]archiveRecord, error)
	ArchiveOpen(context.Context, string, string) (io.ReadCloser, string, error)
	ArchiveDelete(context.Context, string, string) error
}

func (s *Service) archives(ctx context.Context) (archiveStore, error) {
	store, err := s.live(ctx)
	if err != nil {
		return nil, err
	}
	archives, ok := store.(archiveStore)
	if !ok {
		return nil, errors.New("target does not support portable archives")
	}
	return archives, nil
}

func archiveScope(reg Registration) string { return path.Join(reg.Dataset, reg.SourceID) }
func archiveKey(scope, id string) string   { return path.Join("Snapshots", "tar", scope, id) }
func sortArchives(records []archiveRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].ID > records[j].ID
		}
		return records[i].CreatedAt.After(records[j].CreatedAt)
	})
}

func (s *s3Store) ArchivePut(ctx context.Context, scope, id string, f *os.File, size int64, hash string) error {
	_, err := s.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: aws.String(s.target.bucket), Key: aws.String(path.Join(s.target.prefix, archiveKey(scope, id))),
		Body: f, ContentLength: aws.Int64(size), ContentType: aws.String("application/gzip"), IfNoneMatch: aws.String("*"),
		Metadata: map[string]string{"keepsync-sha256": hash, "keepsync-format": "tar"},
	})
	return err
}

func (s *s3Store) ArchiveList(ctx context.Context, scope string) ([]archiveRecord, error) {
	prefix := path.Join(s.target.prefix, archiveKey(scope, "")) + "/"
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.target.bucket), Prefix: aws.String(prefix)})
	records := []archiveRecord{}
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			id := path.Base(key)
			if key != prefix+id || !validArchiveID(id) {
				continue
			}
			created, err := archiveTime(id)
			if err != nil {
				continue
			}
			records = append(records, archiveRecord{ID: id, Key: path.Join(s.target.prefix, archiveKey(scope, id)), CreatedAt: created, Size: aws.ToInt64(object.Size), Format: "tar"})
		}
	}
	sortArchives(records)
	return records, nil
}

func (s *s3Store) ArchiveOpen(ctx context.Context, scope, id string) (io.ReadCloser, string, error) {
	if !validArchiveID(id) {
		return nil, "", errors.New("invalid archive ID")
	}
	r, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.target.bucket), Key: aws.String(path.Join(s.target.prefix, archiveKey(scope, id)))})
	if err != nil {
		return nil, "", err
	}
	return r.Body, r.Metadata["keepsync-sha256"], nil
}

func (s *s3Store) ArchiveDelete(ctx context.Context, scope, id string) error {
	if !validArchiveID(id) {
		return errors.New("invalid archive ID")
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.target.bucket), Key: aws.String(path.Join(s.target.prefix, archiveKey(scope, id)))})
	return err
}

func (s *localStore) archiveDir(scope string) (string, error) {
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

func (s *localStore) ArchivePut(ctx context.Context, scope, id string, f *os.File, size int64, hash string) error {
	if !validArchiveID(id) {
		return errors.New("invalid archive ID")
	}
	dir, err := s.archiveDir(scope)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	stage, err := os.CreateTemp(dir, ".pending-*")
	if err != nil {
		return err
	}
	defer os.Remove(stage.Name())
	n, err := io.Copy(stage, &contextReader{ctx, f})
	if err == nil && n != size {
		err = io.ErrUnexpectedEOF
	}
	err = errors.Join(err, stage.Sync(), stage.Close())
	if err != nil {
		return err
	}
	// A hard link publishes atomically and, unlike Rename, cannot overwrite a
	// previous recovery point if an archive ID ever collides.
	if err := os.Link(stage.Name(), filepath.Join(dir, id)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *localStore) ArchiveList(ctx context.Context, scope string) ([]archiveRecord, error) {
	dir, err := s.archiveDir(scope)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []archiveRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	records := []archiveRecord{}
	for {
		entries, readErr := f.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !entry.Type().IsRegular() || !validArchiveID(entry.Name()) {
				continue
			}
			created, err := archiveTime(entry.Name())
			if err != nil {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			records = append(records, archiveRecord{ID: entry.Name(), Key: archiveKey(scope, entry.Name()), CreatedAt: created, Size: info.Size(), Format: "tar"})
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	sortArchives(records)
	return records, nil
}

func (s *localStore) ArchiveOpen(ctx context.Context, scope, id string) (io.ReadCloser, string, error) {
	if !validArchiveID(id) {
		return nil, "", errors.New("invalid archive ID")
	}
	dir, err := s.archiveDir(scope)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", err
	}
	defer root.Close()
	info, err := root.Lstat(id)
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("archive %s is not a regular file", id)
	}
	f, err := root.Open(id)
	return f, "", err
}

func (s *localStore) ArchiveDelete(ctx context.Context, scope, id string) error {
	if !validArchiveID(id) {
		return errors.New("invalid archive ID")
	}
	dir, err := s.archiveDir(scope)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	err = root.Remove(id)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
