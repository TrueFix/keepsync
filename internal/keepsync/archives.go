package keepsync

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const archiveClock = "20060102T150405.000000000Z"

var archivePattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[0-9a-f]{16}\.tar\.gz$`)

func validArchiveID(id string) bool { return archivePattern.MatchString(id) }
func archiveTime(id string) (time.Time, error) {
	if !validArchiveID(id) {
		return time.Time{}, errors.New("invalid archive ID")
	}
	return time.Parse(archiveClock, id[:len(archiveClock)])
}

type archiveManifest struct {
	Version   int       `json:"version"`
	Format    string    `json:"format"`
	ID        string    `json:"id"`
	Dataset   string    `json:"dataset"`
	SourceID  string    `json:"source_id"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) createTarSnapshot(ctx context.Context, source string, reg Registration) (archiveRecord, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return archiveRecord{}, err
	}
	created := time.Now().UTC()
	id := created.Format(archiveClock) + "-" + hex.EncodeToString(random[:]) + ".tar.gz"
	manifest := archiveManifest{Version: 1, Format: "tar.gz", ID: id, Dataset: reg.Dataset, SourceID: reg.SourceID, CreatedAt: created}
	f, err := os.CreateTemp(s.cfg.StateDir, ".archive-*")
	if err != nil {
		return archiveRecord{}, err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	s.log.Info("Snapshot started", "format", "tar", "snapshot", id)
	hash := sha256.New()
	gz, err := gzip.NewWriterLevel(io.MultiWriter(f, hash), gzip.BestSpeed)
	if err != nil {
		return archiveRecord{}, err
	}
	tw := tar.NewWriter(gz)
	err = s.writeTar(ctx, source, tw, manifest)
	err = errors.Join(err, tw.Close(), gz.Close())
	if err != nil {
		return archiveRecord{}, err
	}
	if err := f.Sync(); err != nil {
		return archiveRecord{}, err
	}
	if _, err := s.source(ctx); err != nil {
		return archiveRecord{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return archiveRecord{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return archiveRecord{}, err
	}
	store, err := s.archives(ctx)
	if err != nil {
		return archiveRecord{}, err
	}
	if err := store.ArchivePut(ctx, archiveScope(reg), id, f, info.Size(), hex.EncodeToString(hash.Sum(nil))); err != nil {
		return archiveRecord{}, fmt.Errorf("upload archive: %w", err)
	}
	return archiveRecord{ID: id, Key: archiveKey(archiveScope(reg), id), CreatedAt: created, Size: info.Size(), Format: "tar"}, nil
}

func (s *Service) writeTar(ctx context.Context, source string, tw *tar.Writer, manifest archiveManifest) error {
	metadata, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "keepsync.json", Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(metadata)), ModTime: manifest.CreatedAt, Format: tar.FormatPAX}); err != nil {
		return err
	}
	if _, err := tw.Write(metadata); err != nil {
		return err
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	files := 0
	markerSeen := false
	err = walkTree(ctx, source, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		// The identity marker is metadata required for recovering the same source.
		if rel != markerName && s.excluded(rel, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if rel == markerName {
			markerSeen = true
		}
		info, err := root.Lstat(filepath.FromSlash(rel))
		if err != nil {
			return err
		}
		if d.IsDir() != info.IsDir() {
			return fmt.Errorf("source entry changed type: %q", rel)
		}
		name := "data"
		if rel != "." {
			name += "/" + rel
		}
		if !safeArchiveName(name) {
			return fmt.Errorf("source path cannot be stored safely in a portable TAR: %q", rel)
		}
		switch {
		case info.IsDir():
			h, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			h.Name = name
			h.Format = tar.FormatPAX
			return tw.WriteHeader(h)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := root.Readlink(filepath.FromSlash(rel))
			if err != nil {
				return err
			}
			if !safeArchiveLink(name, link) {
				return fmt.Errorf("TAR cannot include a link outside data/: %q", rel)
			}
			h, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			h.Name = name
			h.Format = tar.FormatPAX
			return tw.WriteHeader(h)
		case info.Mode().IsRegular():
			f, err := root.Open(filepath.FromSlash(rel))
			if err != nil {
				return err
			}
			defer f.Close()
			before, err := f.Stat()
			if err != nil {
				return err
			}
			if !os.SameFile(info, before) || !before.Mode().IsRegular() {
				return fmt.Errorf("source changed while opening %q", rel)
			}
			h, err := tar.FileInfoHeader(before, "")
			if err != nil {
				return err
			}
			h.Name = name
			h.Format = tar.FormatPAX
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if _, err := io.CopyN(tw, &contextReader{ctx, f}, before.Size()); err != nil {
				return fmt.Errorf("archive %q: %w", rel, err)
			}
			after, err := f.Stat()
			if err != nil {
				return err
			}
			current, err := root.Lstat(filepath.FromSlash(rel))
			if err != nil {
				return err
			}
			bc, _ := fileMetadata(before)
			ac, _ := fileMetadata(after)
			if !os.SameFile(before, current) || !current.Mode().IsRegular() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || bc != ac {
				return fmt.Errorf("file changed while archiving %q; snapshot not published", rel)
			}
			if rel != markerName {
				files++
			}
			return nil
		default:
			return fmt.Errorf("TAR does not support special source entry %q", rel)
		}
	})
	if err != nil {
		return err
	}
	if files == 0 {
		return errors.New("archive contains no eligible regular files")
	}
	if !markerSeen {
		if err := tw.WriteHeader(&tar.Header{Name: "data/" + markerName, Typeflag: tar.TypeReg, Mode: 0444, Size: int64(len(manifest.SourceID)), ModTime: manifest.CreatedAt, Format: tar.FormatPAX}); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(manifest.SourceID)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) listTar(ctx context.Context) error {
	reg, err := s.registration(ctx)
	if err != nil {
		return err
	}
	store, err := s.archives(ctx)
	if err != nil {
		return err
	}
	records, err := store.ArchiveList(ctx, archiveScope(reg))
	if err != nil {
		return err
	}
	return json.NewEncoder(s.out).Encode(records)
}

func (s *Service) retainTar(ctx context.Context, reg Registration, protected string) error {
	if !validArchiveID(protected) {
		return errors.New("retention requires a confirmed archive ID")
	}
	store, err := s.archives(ctx)
	if err != nil {
		return err
	}
	scope := archiveScope(reg)
	records, err := store.ArchiveList(ctx, scope)
	if err != nil {
		return err
	}
	found := false
	for _, record := range records {
		if record.ID == protected {
			found = true
			break
		}
	}
	if !found {
		return errors.New("confirmed archive missing; retention blocked")
	}
	// Keep the just-confirmed point even if the source machine's clock moved back.
	remaining := s.cfg.Snapshots.KeepLast - 1
	for _, record := range records {
		if record.ID == protected {
			continue
		}
		if remaining > 0 {
			remaining--
			continue
		}
		if err := store.ArchiveDelete(ctx, scope, record.ID); err != nil {
			return err
		}
	}
	s.log.Info("Snapshot retention complete", "format", "tar", "keep_last", s.cfg.Snapshots.KeepLast)
	return nil
}

func (s *Service) downloadTar(ctx context.Context, reg Registration, id string) (*os.File, error) {
	store, err := s.archives(ctx)
	if err != nil {
		return nil, err
	}
	body, expected, err := store.ArchiveOpen(ctx, archiveScope(reg), id)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	f, err := os.CreateTemp(s.cfg.StateDir, ".archive-download-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() { f.Close(); os.Remove(f.Name()) }
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hash), &contextReader{ctx, body}); err != nil {
		cleanup()
		return nil, err
	}
	if expected != "" && hex.EncodeToString(hash.Sum(nil)) != expected {
		cleanup()
		return nil, errors.New("archive SHA-256 mismatch")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, err
	}
	return f, nil
}

func (s *Service) checkTar(ctx context.Context) error {
	reg, err := s.registration(ctx)
	if err != nil {
		return err
	}
	store, err := s.archives(ctx)
	if err != nil {
		return err
	}
	records, err := store.ArchiveList(ctx, archiveScope(reg))
	if err != nil {
		return err
	}
	for _, record := range records {
		f, err := s.downloadTar(ctx, reg, record.ID)
		if err != nil {
			return err
		}
		err = readTar(ctx, f, reg, record.ID, nil)
		f.Close()
		os.Remove(f.Name())
		if err != nil {
			return fmt.Errorf("check %s: %w", record.ID, err)
		}
	}
	s.log.Info("Archive integrity check complete", "snapshots", len(records))
	return nil
}

func (s *Service) restoreTar(ctx context.Context, reg Registration, id, dest string) error {
	f, err := s.downloadTar(ctx, reg, id)
	if err != nil {
		return err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	base, err := resolved(s.cfg.RestoreRoot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer root.Close()
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	stageName := ".keepsync-restore-" + hex.EncodeToString(random[:])
	if err := root.Mkdir(stageName, 0700); err != nil {
		return err
	}
	defer root.RemoveAll(stageName)
	stage, err := root.OpenRoot(stageName)
	if err != nil {
		return err
	}
	err = readTar(ctx, f, reg, id, stage)
	err = errors.Join(err, stage.Close())
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(base, dest)
	if err != nil || rel == "." || !within(base, dest) {
		return errors.New("invalid restore destination")
	}
	if err := root.MkdirAll(filepath.Dir(rel), 0700); err != nil {
		return err
	}
	parent, err := root.Open(filepath.Dir(rel))
	if err != nil {
		return err
	}
	// AT_REMOVEDIR can remove only an empty directory, never a file that may
	// have appeared concurrently. Some filesystems reject rename over a directory.
	err = unix.Unlinkat(int(parent.Fd()), filepath.Base(rel), unix.AT_REMOVEDIR)
	parent.Close()
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("restore destination is not empty: %w", err)
	}
	// No destination is exposed
	// until the complete archive, identity and gzip checksum have been verified.
	if err := root.Rename(stageName, rel); err != nil {
		return fmt.Errorf("publish restored directory: %w", err)
	}
	s.log.Info("Archive restored", "snapshot", id, "target", dest)
	return nil
}

func safeArchiveName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "\\\x00:") && !strings.HasPrefix(name, "/") && path.Clean(name) == name && (name == "data" || strings.HasPrefix(name, "data/"))
}
func safeArchiveLink(name, target string) bool {
	if target == "" || strings.ContainsAny(target, "\\\x00:") || path.IsAbs(target) {
		return false
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	return resolved == "data" || strings.HasPrefix(resolved, "data/")
}

func checkArchiveParents(root *os.Root, name string) error {
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		info, err := root.Lstat(filepath.FromSlash(parent))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("archive entry traverses a non-directory or symlink")
		}
	}
	return nil
}

// Validate while streaming into a private directory, or discard payloads during
// check. No archive-wide filename/content map is retained in memory.
func readTar(ctx context.Context, input io.Reader, reg Registration, id string, output *os.Root) error {
	buffer := bufio.NewReader(&contextReader{ctx, input})
	gz, err := gzip.NewReader(buffer)
	if err != nil {
		return err
	}
	defer gz.Close()
	gz.Multistream(false)
	tr := tar.NewReader(gz)
	first, err := tr.Next()
	if err != nil {
		return err
	}
	if first.Name != "keepsync.json" || first.Typeflag != tar.TypeReg || first.Size < 1 || first.Size > 64<<10 {
		return errors.New("archive has no valid KeepSync manifest")
	}
	metadata, err := io.ReadAll(tr)
	if err != nil {
		return err
	}
	var manifest archiveManifest
	decoder := json.NewDecoder(bytes.NewReader(metadata))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("invalid manifest JSON")
	}
	created, err := archiveTime(id)
	if err != nil {
		return err
	}
	if manifest.Version != 1 || manifest.Format != "tar.gz" || manifest.ID != id || manifest.Dataset != reg.Dataset || manifest.SourceID != reg.SourceID || !manifest.CreatedAt.Equal(created) {
		return errors.New("archive identity does not match this dataset/source")
	}
	if output != nil {
		f, err := output.OpenFile("keepsync.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(metadata)
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
	}
	files := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		name := h.Name
		if h.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if !safeArchiveName(name) {
			return fmt.Errorf("unsafe archive path %q", h.Name)
		}
		if output != nil {
			if err := checkArchiveParents(output, name); err != nil {
				return err
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if h.Size != 0 {
				return errors.New("directory with unexpected content")
			}
			if output != nil {
				if info, err := output.Lstat(filepath.FromSlash(name)); err == nil && !info.IsDir() {
					return errors.New("directory conflicts with another archive entry")
				}
				if err := output.MkdirAll(filepath.FromSlash(name), 0700); err != nil {
					return err
				}
			}
		case tar.TypeReg, tar.TypeRegA:
			if name == "data" {
				return errors.New("data root is not a directory")
			}
			if output == nil {
				if _, err := io.Copy(io.Discard, &contextReader{ctx, tr}); err != nil {
					return err
				}
			} else {
				if err := output.MkdirAll(filepath.FromSlash(path.Dir(name)), 0700); err != nil {
					return err
				}
				f, err := output.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if err != nil {
					return err
				}
				_, err = io.Copy(f, &contextReader{ctx, tr})
				err = errors.Join(err, f.Close())
				if err != nil {
					return err
				}
				if err := output.Chmod(filepath.FromSlash(name), os.FileMode(h.Mode)&0777); err != nil {
					return err
				}
			}
			if name != "data/"+markerName {
				files++
			}
		case tar.TypeSymlink:
			if name == "data" || h.Size != 0 || !safeArchiveLink(name, h.Linkname) {
				return errors.New("archive symlink escapes data/")
			}
			if output != nil {
				if err := output.MkdirAll(filepath.FromSlash(path.Dir(name)), 0700); err != nil {
					return err
				}
				if err := output.Symlink(filepath.FromSlash(h.Linkname), filepath.FromSlash(name)); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unsupported archive entry type %d", h.Typeflag)
		}
	}
	if files == 0 {
		return errors.New("archive contains no user files")
	}
	// Consume through the gzip trailer; tar EOF alone does not validate its CRC.
	tail := make([]byte, 32<<10)
	for {
		n, err := gz.Read(tail)
		for _, b := range tail[:n] {
			if b != 0 {
				return errors.New("unexpected data after TAR end marker")
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if _, err := buffer.Peek(1); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("unexpected trailing compressed data")
	}
	return nil
}
