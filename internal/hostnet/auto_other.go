//go:build !linux

// Package hostnet's `auto` mode is the TAP + iptables NAT path used on Linux
// hosts for `--net auto`. On non-Linux hosts (Windows, macOS) gocracker uses
// the userspace slirp engine instead, so this stub gives non-Linux callers
// type/function compatibility while making any actual use return a clear
// "not supported on this platform" error.
package hostnet

import "errors"

var errNotSupported = errors.New("hostnet auto mode is Linux-only; use --net slirp on Windows/macOS")

// AutoNetwork mirrors the Linux export so callers compile on every platform.
// All methods return the not-supported error or zero values.
type AutoNetwork struct{}

// NewAuto always returns errNotSupported on non-Linux platforms.
func NewAuto(project, tapName string) (*AutoNetwork, error) { return nil, errNotSupported }

func (n *AutoNetwork) TapName() string           { return "" }
func (n *AutoNetwork) GuestCIDR() string         { return "" }
func (n *AutoNetwork) GuestIP() string           { return "" }
func (n *AutoNetwork) GatewayIP() string         { return "" }
func (n *AutoNetwork) UpstreamInterface() string { return "" }
func (n *AutoNetwork) Activate() error           { return errNotSupported }
func (n *AutoNetwork) Close()                    {}
