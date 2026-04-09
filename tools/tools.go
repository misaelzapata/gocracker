//go:build tools

package tools

import (
	// Keep generate-time dependencies required by internal/guest/init.go.
	_ "github.com/vishvananda/netlink"
)
