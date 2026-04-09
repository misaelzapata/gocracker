# Security Policy

## Reporting a Vulnerability

Please do not file public GitHub issues for suspected vulnerabilities.

Report security issues privately to the project maintainers with:

- affected version or commit
- host/kernel details
- reproduction steps
- expected vs actual behavior
- impact assessment if known

Until a dedicated private inbox is published, treat direct maintainer contact as
the reporting path and avoid public disclosure before triage.

## Scope

Security-sensitive areas include:

- guest memory and virtio device emulation
- OCI extraction and build isolation
- jailer / namespace / cgroup setup
- API authentication and path validation
- snapshot / migration artifacts

## Supported Versions

Security fixes are applied to the current mainline code in this repository.
