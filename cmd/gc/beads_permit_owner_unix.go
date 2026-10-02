//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package main

import (
	"os"
	"syscall"
)

func hostBeadsPermitOwnerIsTrusted(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && hostBeadsPermitOwnerUIDIsTrusted(uint64(stat.Uid), os.Geteuid())
}

func hostBeadsPermitOwnerIsRoot(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && hostBeadsPermitOwnerUIDIsRoot(uint64(stat.Uid))
}
