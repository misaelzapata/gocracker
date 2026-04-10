# Contributing

## Scope

`gocracker` is Linux-only and targets KVM-backed microVM workflows. Contributions
should preserve the project's current priorities:

- correctness before features
- reproducible tests before wide claims in docs
- explicit failure over silent partial support

## Development Setup

1. Install Go and the Linux host dependencies needed by the README.
2. Build the local binaries:

```bash
go build ./cmd/gocracker ./cmd/gocracker-jailer ./cmd/gocracker-vmm
```

3. Run the baseline test suite:

```bash
go test ./...
```

4. For privileged integration runs, use the shipped guest kernel:

```bash
sudo -n env GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux \
  go test -tags integration ./tests/integration -count=1 -v
```

## Change Guidelines

- Prefer small, reviewable patches over large mixed refactors.
- When changing runtime behavior, update `README.md` in the same change.
- If a feature is partial, fail explicitly instead of silently accepting config.
- Preserve the external-repos and manual-smoke harnesses; they are part of the
  regression gates, not optional examples.

## Tests

At minimum, submitters should run:

```bash
go test ./...
```

If the change touches VM runtime, networking, exec, ballooning, or `serve`,
also run the relevant privileged integration tests and document what ran.

## Security

Do not open public issues for suspected vulnerabilities. Follow
[`SECURITY.md`](./SECURITY.md).
