package keepsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
)

type target struct{ local, bucket, prefix, endpoint string }

func parseTarget(value, endpoint string) (target, error) {
	var t target
	if filepath.IsAbs(value) {
		if filepath.Clean(value) == "/" {
			return t, errors.New("target cannot be the filesystem root")
		}
		t.local = filepath.Clean(value)
		return t, nil
	}
	if strings.HasPrefix(value, "s3://") {
		u, err := url.Parse(value)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return t, errors.New("target must be s3://bucket/prefix without credentials or query parameters")
		}
		t.bucket, t.prefix, t.endpoint = u.Host, strings.Trim(u.Path, "/"), endpoint
	} else if strings.HasPrefix(value, "s3:") {
		v := strings.TrimPrefix(value, "s3:")
		if !strings.Contains(v, "://") {
			v = "https://" + v
		}
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return t, errors.New("invalid S3 repository URL")
		}
		parts := strings.SplitN(strings.Trim(u.Path, "/"), "/", 2)
		t.bucket = parts[0]
		if len(parts) == 2 {
			t.prefix = parts[1]
		}
		t.endpoint = u.Scheme + "://" + u.Host
		if endpoint != "" {
			t.endpoint = endpoint
		}
	} else {
		return t, errors.New("KEEPSYNC_TARGET must be s3://bucket/prefix (or an absolute path for local testing)")
	}
	if t.bucket == "" || strings.ContainsAny(t.bucket, "/\\:") {
		return t, errors.New("invalid target bucket")
	}
	for _, part := range strings.Split(t.prefix, "/") {
		if part == ".." || part == "." || strings.Contains(part, "\\") {
			return t, errors.New("invalid target prefix")
		}
	}
	if t.endpoint != "" {
		u, err := url.Parse(t.endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return t, errors.New("S3 endpoint must be an http(s) origin without credentials or a path")
		}
	}
	return t, nil
}

func (t target) snapshots() string {
	if t.local != "" {
		return filepath.Join(t.local, "Snapshots")
	}
	endpoint := strings.TrimSuffix(t.endpoint, "/")
	if endpoint == "" {
		endpoint = "https://s3.amazonaws.com"
	}
	return "s3:" + endpoint + "/" + t.bucket + "/" + path.Join(t.prefix, "Snapshots")
}

type liveStore interface {
	Empty(context.Context) (bool, error)
	Bind(context.Context, string) error
	Identity(context.Context) (string, error)
	Repair(context.Context, string) error
	Matches(context.Context, string, fileEntry) (bool, error)
	Upload(context.Context, string, *os.File, fileEntry) error
	Delete(context.Context, string) error
}

func (s *s3Store) Empty(ctx context.Context) (bool, error) {
	prefix := s.target.prefix
	if prefix != "" {
		prefix += "/"
	}
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.target.bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(128)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return false, err
		}
		for _, object := range page.Contents {
			if aws.ToInt64(object.Size) != 0 || !strings.HasSuffix(aws.ToString(object.Key), "/") {
				return false, nil
			}
		}
	}
	return true, nil
}

func (s *localStore) Empty(ctx context.Context) (bool, error) {
	base := filepath.Dir(s.root)
	if _, err := os.Stat(base); errors.Is(err, os.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	empty := true
	err := walkTree(ctx, base, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			empty = false
			return fs.SkipAll
		}
		return nil
	})
	return empty, err
}

func (s *Service) live(ctx context.Context) (liveStore, error) {
	if s.store != nil {
		return s.store, nil
	}
	t, err := parseTarget(s.cfg.Repository, s.cfg.S3Endpoint)
	if err != nil {
		return nil, err
	}
	if t.local != "" {
		s.store = &localStore{filepath.Join(t.local, "Live")}
		return s.store, nil
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	cfg.Logger = sdkLogger{s.log}
	if cfg.Region == "" {
		cfg.Region = os.Getenv("AWS_DEFAULT_REGION")
		if cfg.Region == "" {
			cfg.Region = "us-east-1"
		}
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if t.endpoint != "" {
			o.BaseEndpoint = aws.String(strings.TrimSuffix(t.endpoint, "/"))
			o.UsePathStyle = true
		}
	})
	s.store = &s3Store{t, client, transfermanager.New(client, func(o *transfermanager.Options) {
		o.Concurrency = 1
		o.PartSizeBytes = 5 << 20
		o.MultipartUploadThreshold = 8 << 20
	})}
	return s.store, nil
}

type s3Store struct {
	target   target
	client   *s3.Client
	uploader *transfermanager.Client
}

type sdkLogger struct{ log *slog.Logger }

func (l sdkLogger) Logf(classification logging.Classification, format string, args ...interface{}) {
	level := slog.LevelDebug
	if classification == logging.Warn {
		level = slog.LevelWarn
	}
	ctx := context.Background()
	if l.log.Enabled(ctx, level) {
		l.log.Log(ctx, level, fmt.Sprintf(format, args...))
	}
}

func (s *s3Store) key(rel string) string { return path.Join(s.target.prefix, "Live", rel) }
func (s *s3Store) Identity(ctx context.Context) (string, error) {
	r, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.target.bucket), Key: aws.String(s.key(markerName))})
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, 65))
	return string(b), err
}
func missingObject(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound" || api.ErrorCode() == "404")
}

// Repair recreates a missing identity marker that local state already
// trusts. Unlike Bind, it never inspects Live/ for other objects: the caller
// already proved ownership by presenting the exact ID from registration.json,
// so this only restores what accidental deletion removed, never claims a
// target local state doesn't already know about.
func (s *s3Store) Repair(ctx context.Context, id string) error {
	existing, err := s.Identity(ctx)
	if err == nil {
		if existing != id {
			return errors.New("Live source identity belongs to another source")
		}
		return nil
	}
	if !missingObject(err) {
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.target.bucket), Key: aws.String(s.key(markerName)), Body: bytes.NewReader([]byte(id)), IfNoneMatch: aws.String("*")})
	if err == nil {
		return nil
	}
	// Someone else created a marker between our read and write; verify it is
	// still ours rather than assuming the write failure means an error.
	existing, idErr := s.Identity(ctx)
	if idErr != nil {
		return err
	}
	if existing != id {
		return errors.New("Live source identity belongs to another source")
	}
	return nil
}
func (s *s3Store) Bind(ctx context.Context, id string) error {
	existing, err := s.Identity(ctx)
	if err == nil {
		if existing != id {
			return errors.New("Live source identity belongs to another source")
		}
		return nil
	}
	if !missingObject(err) {
		return err
	}
	list, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(s.target.bucket), Prefix: aws.String(path.Join(s.target.prefix, "Live") + "/"), MaxKeys: aws.Int32(1)})
	if err != nil {
		return err
	}
	if len(list.Contents) > 0 {
		return errors.New("Live target is non-empty without a source identity; use a fresh target prefix")
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.target.bucket), Key: aws.String(s.key(markerName)), Body: bytes.NewReader([]byte(id)), IfNoneMatch: aws.String("*")})
	return err
}
func (s *s3Store) Matches(ctx context.Context, rel string, entry fileEntry) (bool, error) {
	r, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.target.bucket), Key: aws.String(s.key(rel))})
	if missingObject(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return aws.ToInt64(r.ContentLength) == entry.Size && r.Metadata["keepsync-sha256"] == entry.Hash, nil
}
func (s *s3Store) Upload(ctx context.Context, rel string, f *os.File, entry fileEntry) error {
	_, err := s.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{Bucket: aws.String(s.target.bucket), Key: aws.String(s.key(rel)), Body: f, ContentLength: aws.Int64(entry.Size), Metadata: map[string]string{"keepsync-sha256": entry.Hash}})
	return err
}
func (s *s3Store) Delete(ctx context.Context, rel string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.target.bucket), Key: aws.String(s.key(rel))})
	return err
}

// A filesystem target exercises the same Live/Snapshots contract in tests.
type localStore struct{ root string }

func (s *localStore) Identity(_ context.Context) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.root, markerName))
	return string(b), err
}
func (s *localStore) Bind(ctx context.Context, id string) error {
	if err := os.MkdirAll(s.root, 0700); err != nil {
		return err
	}
	existing, err := s.Identity(ctx)
	if err == nil {
		if existing != id {
			return errors.New("Live source identity belongs to another source")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return errors.New("Live target is non-empty without a source identity")
	}
	f, err := os.OpenFile(filepath.Join(s.root, markerName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(id)
	return errors.Join(err, f.Sync(), f.Close())
}
func (s *localStore) Repair(ctx context.Context, id string) error {
	existing, err := s.Identity(ctx)
	if err == nil {
		if existing != id {
			return errors.New("Live source identity belongs to another source")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(s.root, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.root, markerName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, idErr := s.Identity(ctx)
			if idErr != nil {
				return err
			}
			if existing != id {
				return errors.New("Live source identity belongs to another source")
			}
			return nil
		}
		return err
	}
	_, err = f.WriteString(id)
	return errors.Join(err, f.Sync(), f.Close())
}
func (s *localStore) Matches(_ context.Context, rel string, entry fileEntry) (bool, error) {
	f, err := os.Open(filepath.Join(s.root, filepath.FromSlash(rel)))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	got, err := fingerprint(f)
	return got.Hash == entry.Hash && got.Size == entry.Size, err
}
func (s *localStore) Upload(ctx context.Context, rel string, f *os.File, entry fileEntry) error {
	dest := filepath.Join(s.root, filepath.FromSlash(rel))
	parent, err := resolved(filepath.Dir(dest))
	if err != nil {
		return err
	}
	base, err := resolved(s.root)
	if err != nil {
		return err
	}
	if !within(base, parent) {
		return errors.New("Live path escapes target")
	}
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	stage, err := os.CreateTemp(parent, ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(stage.Name())
	_, err = io.Copy(stage, &contextReader{ctx, f})
	err = errors.Join(err, stage.Sync(), stage.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(stage.Name(), dest); err != nil {
		return fmt.Errorf("commit Live file: %w", err)
	}
	return nil
}
func (s *localStore) Delete(_ context.Context, rel string) error {
	err := os.Remove(filepath.Join(s.root, filepath.FromSlash(rel)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
