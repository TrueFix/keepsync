package keepsync

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Read directories in small batches instead of sorting every entry into RAM.
func walkTree(ctx context.Context, root string, visit fs.WalkDirFunc) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	var walk func(string, fs.DirEntry) error
	walk = func(p string, entry fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(p, entry, nil); err != nil {
			if err == fs.SkipDir && entry.IsDir() {
				return nil
			}
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		for {
			entries, readErr := f.ReadDir(128)
			for _, child := range entries {
				if err := walk(filepath.Join(p, child.Name()), child); err != nil {
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
	err = walk(root, fs.FileInfoToDirEntry(info))
	if err == fs.SkipAll {
		return nil
	}
	return err
}
