# Changelog

All notable changes to `gocracker` will be documented in this file.

## Unreleased

### Added

- Bearer-token support for the HTTP API client/server path.
- Trusted-path validation for API-supplied kernel, workspace, and snapshot paths.
- Release-readiness project files: `LICENSE`, `CONTRIBUTING.md`, `SECURITY.md`,
  and CI workflow scaffolding.

### Changed

- Hardened virtio guest memory access and descriptor-chain traversal.
- Propagated real flush failures in virtio-blk.
- Hardened OCI layer extraction to keep entries inside the rootfs.
- Hardened jailer file/directory creation against symlink-based path tricks.

### Fixed

- vCPU mmap and guest-RAM cleanup gaps in KVM resource teardown.
- virtio-fs eventfd leak on partial allocation failure.
