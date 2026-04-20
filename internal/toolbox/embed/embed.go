// Package embed exposes the pre-built static toolbox agent binary
// (cmd/gocracker-toolbox) so that pkg/container/ can inject it into
// every guest disk gocracker builds. Mirrors the established pattern
// from internal/guest (init_embed_*.go) — host arch == guest arch
// always (KVM cross-arch is not supported), so we ship one arch's
// binary per gocracker build, gated by build tags.
//
// Constants like BinaryPath / Version live in internal/toolbox/spec
// because internal/guest/init.go needs the path string but must NOT
// pull in the embedded binary bytes (~6 MB) by transitive import.
package embed
