//go:build !linux

package keepsync

import "os"

func fileMetadata(info os.FileInfo) (int64, string) { return 0, "" }
