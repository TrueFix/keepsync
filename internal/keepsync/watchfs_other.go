//go:build !linux

package keepsync

func pollingPreferred(path string) bool { return false }
