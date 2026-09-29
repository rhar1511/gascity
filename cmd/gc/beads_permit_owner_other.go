//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris

package main

import "os"

// Platforms without a supported owner-UID source reject configured authority
// files. Accepting an unknown owner would weaken the host-only key boundary.
func hostBeadsPermitOwnerIsTrusted(os.FileInfo) bool { return false }

func hostBeadsPermitOwnerIsRoot(os.FileInfo) bool { return false }
