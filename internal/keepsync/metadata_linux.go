//go:build linux

package keepsync

import (
	"fmt"
	"os"
	"syscall"
)

func fileMetadata(info os.FileInfo) (int64, string) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(stat.Ctim.Sec)*1e9 + int64(stat.Ctim.Nsec), fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
	}
	return 0, ""
}
