//go:build darwin

package oci

import (
	"syscall"
	"time"
)

func statAccessTime(stat *syscall.Stat_t) time.Time {
	return time.Unix(stat.Atimespec.Sec, stat.Atimespec.Nsec)
}

func statChangeTime(stat *syscall.Stat_t) time.Time {
	return time.Unix(stat.Ctimespec.Sec, stat.Ctimespec.Nsec)
}
