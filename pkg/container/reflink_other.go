//go:build !linux

package container

import "errors"

// errReflinkUnavailable signals that the host kernel/filesystem cannot do a
// FICLONE reflink. The caller in copyDiskImage falls back to a full byte
// copy on this error, so no functional regression — Windows just doesn't
// get the CoW fast path.
var errReflinkUnavailable = errors.New("reflink (FICLONE) not supported on this platform")

func tryReflink(src, dst string) error { return errReflinkUnavailable }
