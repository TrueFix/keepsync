//go:build linux

package keepsync

import "golang.org/x/sys/unix"

func pollingPreferred(path string) bool {
	var info unix.Statfs_t
	if unix.Statfs(path, &info) != nil {
		return false
	}
	// Network/FUSE/9P mounts often do not propagate writes to inotify watchers
	// in a different mount namespace (Docker Desktop and remote volumes).
	switch uint64(info.Type) {
	case 0x6969, 0x517b, 0xff534d42, 0x65735546, 0x01021997:
		return true
	}
	return false
}
