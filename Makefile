BIN      := gocracker
MODULE   := github.com/gocracker/gocracker
CMD      := ./cmd/gocracker
TARGET_GOOS ?= linux
TARGET_GOARCH ?= $(shell go env GOARCH)

# Windows binaries get a .exe suffix; everything else has no suffix. Lets a
# single `$(BIN)$(BIN_EXT)` work across platforms in build commands.
ifeq ($(TARGET_GOOS),windows)
  BIN_EXT := .exe
else
  BIN_EXT :=
endif

# Version stamp injected via -ldflags. VERSION takes the git tag if
# the working tree is clean at a tag, else "dev-<short-sha>-dirty?".
# COMMIT is the short SHA; DATE is ISO-8601 UTC. Override from the
# command line for release builds (e.g. `make build VERSION=v1.2.3`).
VERSION ?= $(shell git describe --tags --exact-match --dirty=-dirty 2>/dev/null || echo "dev-$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)$(shell if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then git diff --quiet 2>/dev/null || echo -dirty; fi)")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_LDFLAGS = -X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
                  -X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
                  -X $(MODULE)/internal/buildinfo.Date=$(DATE)

.PHONY: all build build-amd64 build-arm64 build-windows-amd64 build-darwin-amd64 build-darwin-arm64 generate tidy test test-uds coverage clean kernel-host kernel-host-virtiofs kernel-guest kernel-guest-virtiofs kernel-guest-arm64 kernel-guest-arm64-minimal kernel-unpack hostcheck sandboxes-local sandboxes-local-down sandboxes-local-status sandboxes-local-logs sandboxes-local-seed vet-cross

all: build

## Pre-compile the guest init binary and embed it
generate:
	go generate ./internal/guest/

## Download dependencies, generate, and build all six user-facing binaries.
##
## All six are produced on every TARGET_GOOS so packaging is uniform.
## gocracker-vmm/jailer/hostcheck/toolbox/debugvm have Linux-only main.go
## files (KVM, namespaces, seccomp, /dev/kvm checks); on non-Linux targets
## they fall through to main_other.go stubs that print a clear "Linux-only
## / pending Phase N" message and exit 2. The Windows binaries are real
## once Phases 1.2 + 2 (and later) land.
GO_BUILD = CGO_ENABLED=0 GOOS=$(TARGET_GOOS) GOARCH=$(TARGET_GOARCH) \
  go build -trimpath -ldflags="-s -w $(VERSION_LDFLAGS)"

build: tidy generate
	$(GO_BUILD) -o $(BIN)$(BIN_EXT)               $(CMD)
	$(GO_BUILD) -o gocracker-vmm$(BIN_EXT)        ./cmd/gocracker-vmm
	$(GO_BUILD) -o gocracker-jailer$(BIN_EXT)     ./cmd/gocracker-jailer
	$(GO_BUILD) -o gocracker-hostcheck$(BIN_EXT)  ./cmd/gocracker-hostcheck
	$(GO_BUILD) -o gocracker-toolbox$(BIN_EXT)    ./cmd/gocracker-toolbox
	$(GO_BUILD) -o toolbox-cli$(BIN_EXT)          ./cmd/toolbox-cli
	$(GO_BUILD) -o debugvm$(BIN_EXT)              ./cmd/debugvm

build-amd64:
	$(MAKE) build TARGET_GOARCH=amd64

build-arm64:
	$(MAKE) build TARGET_GOARCH=arm64

build-windows-amd64:
	$(MAKE) build TARGET_GOOS=windows TARGET_GOARCH=amd64

build-darwin-amd64:
	$(MAKE) build TARGET_GOOS=darwin TARGET_GOARCH=amd64

build-darwin-arm64:
	$(MAKE) build TARGET_GOOS=darwin TARGET_GOARCH=arm64

## Run go vet across every supported GOOS/GOARCH so the tree never goes red on
## a cross-compile silently. Each invocation is independent so a regression on
## one target doesn't mask others.
##
## On Linux we vet the whole tree. On non-Linux we vet only the packages that
## should currently cross-compile clean. The non-Linux allow-list will grow
## phase by phase: Phase 1.2 unblocks pkg/vmm; Phase 2 unblocks pkg/container,
## internal/api, etc.; Phase 8 unblocks gocracker-vmm worker.
NONLINUX_VET_PKGS = \
  ./internal/paths/... \
  ./internal/slirp/... \
  ./internal/loader/... \
  ./internal/whp/... \
  ./pkg/vmm/... \
  ./cmd/gocracker/... \
  ./cmd/gocracker-vmm/... \
  ./cmd/gocracker-jailer/... \
  ./cmd/gocracker-hostcheck/... \
  ./cmd/gocracker-toolbox/... \
  ./cmd/toolbox-cli/... \
  ./cmd/debugvm/... \
  ./tools/bench-rtt/... \
  ./sandboxes/cmd/...

## internal/whp casts uintptr (windows.VirtualAlloc result) to
## unsafe.Pointer to build a slice over the VirtualAlloc'd region. That
## memory is NOT Go-managed (the GC never moves it), so the cast is
## safe — but vet's unsafeptr analyzer can't tell. Disable just this
## one analyzer for internal/whp, everywhere else keeps full vet.
vet-cross:
	GOOS=linux   GOARCH=amd64 go vet ./...
	GOOS=linux   GOARCH=arm64 go vet ./...
	GOOS=windows GOARCH=amd64 go vet -unsafeptr=false ./internal/whp/...
	GOOS=windows GOARCH=amd64 go vet $(filter-out ./internal/whp/...,$(NONLINUX_VET_PKGS))
	GOOS=darwin  GOARCH=amd64 go vet $(NONLINUX_VET_PKGS)
	GOOS=darwin  GOARCH=arm64 go vet $(NONLINUX_VET_PKGS)

tidy:
	go mod tidy

test:
	go test ./...

## Unit tests for the Firecracker-style UDS (vsock) feature, under the
## race detector, repeated 10x to surface ordering bugs and goroutine
## leaks. Keep this tight — runs in <10s locally and is cheap for CI.
test-uds:
	go test -race -count=10 \
	  -run 'VsockConfig|ResolveHostSidePath|ResolveWorkerHostSidePath|UDSListener|ParseConnect|SanitizeReason|VM_Cleanup|ApplyVsockUDSPathOverride|StartTXAvailPoller_StopsOnClose|HandleGetVM_VsockUDSPath|BuildVsockConfig_UDSPath' \
	  ./pkg/vmm/... ./pkg/container/... ./internal/vsock/... ./internal/api/...

coverage:
	chmod +x ./tools/coverage-repo.sh && ./tools/coverage-repo.sh

kernel-host:
	./tools/prepare-kernel.sh --profile standard

kernel-host-virtiofs:
	./tools/prepare-kernel.sh --profile virtiofs

kernel-guest:
	./tools/build-guest-kernel.sh --profile standard

kernel-guest-virtiofs:
	./tools/build-guest-kernel.sh --profile virtiofs

kernel-guest-arm64:
	./tools/build-guest-kernel-arm64.sh --profile standard

kernel-guest-arm64-minimal:
	./tools/build-guest-kernel-arm64.sh --profile minimal

## Decompress every artifacts/kernels/*.gz that ships with the repo,
## leaving the uncompressed kernels next to the .gz so `gocracker run
## --kernel artifacts/kernels/gocracker-guest-standard-vmlinux` works
## with no further setup. Idempotent — gunzip -k preserves the .gz.
kernel-unpack:
	@for gz in artifacts/kernels/*.gz; do \
	  if [ -f "$$gz" ]; then \
	    out="$${gz%.gz}"; \
	    if [ ! -f "$$out" ] || [ "$$gz" -nt "$$out" ]; then \
	      echo "  unpack $$gz -> $$out"; \
	      gunzip -kf "$$gz"; \
	    fi; \
	  fi; \
	done

hostcheck:
	go run ./cmd/gocracker-hostcheck

sandboxes-local:
	chmod +x ./tools/sandboxes-local.sh && ./tools/sandboxes-local.sh up

sandboxes-local-down:
	chmod +x ./tools/sandboxes-local.sh && ./tools/sandboxes-local.sh down

sandboxes-local-status:
	chmod +x ./tools/sandboxes-local.sh && ./tools/sandboxes-local.sh status

sandboxes-local-logs:
	chmod +x ./tools/sandboxes-local.sh && ./tools/sandboxes-local.sh logs

sandboxes-local-seed:
	chmod +x ./tools/sandboxes-local.sh && ./tools/sandboxes-local.sh seed

## Run the API server on TCP port 8080 (easier for testing without root)
run: build
	./$(BIN) --api-addr :8080

## Quick curl helpers — set KERNEL and DISK env vars first
start-vm:
	curl -s -X PUT http://localhost:8080/boot-source \
	  -H 'Content-Type: application/json' \
	  -d '{"kernel_image_path":"$(KERNEL)","boot_args":"console=ttyS0 reboot=k panic=1 nomodule i8042.noaux i8042.nomux i8042.dumbkbd swiotlb=noforce"}'
	curl -s -X PUT http://localhost:8080/machine-config \
	  -H 'Content-Type: application/json' \
	  -d '{"vcpu_count":1,"mem_size_mib":256}'
	curl -s -X PUT http://localhost:8080/drives/rootfs \
	  -H 'Content-Type: application/json' \
	  -d '{"drive_id":"rootfs","path_on_host":"$(DISK)","is_root_device":true,"is_read_only":false}'
	curl -s -X PUT http://localhost:8080/network-interfaces/eth0 \
	  -H 'Content-Type: application/json' \
	  -d '{"iface_id":"eth0","host_dev_name":"tap0"}'
	curl -s -X PUT http://localhost:8080/actions \
	  -H 'Content-Type: application/json' \
	  -d '{"action_type":"InstanceStart"}'

clean:
	rm -f $(BIN) $(BIN).exe \
	  gocracker-vmm gocracker-vmm.exe \
	  gocracker-jailer gocracker-jailer.exe \
	  gocracker-hostcheck gocracker-hostcheck.exe \
	  gocracker-toolbox gocracker-toolbox.exe \
	  toolbox-cli toolbox-cli.exe \
	  debugvm debugvm.exe \
	  internal/guest/init_amd64.bin internal/guest/init_arm64.bin
