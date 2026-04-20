BIN      := gocracker
MODULE   := github.com/gocracker/gocracker
CMD      := ./cmd/gocracker
TARGET_GOOS ?= linux
TARGET_GOARCH ?= $(shell go env GOARCH)

.PHONY: all build build-amd64 build-arm64 generate tidy test test-uds coverage clean kernel-host kernel-host-virtiofs kernel-guest kernel-guest-virtiofs hostcheck sandboxes-local sandboxes-local-down sandboxes-local-status sandboxes-local-logs sandboxes-local-seed

all: build

## Pre-compile the guest init binary and embed it
generate:
	go generate ./internal/guest/

## Download dependencies, generate, and build all binaries.
## gocracker-vmm and gocracker-jailer are linux-only (they use KVM, mount
## namespaces, seccomp, etc.) so we skip them when TARGET_GOOS != linux.
build: tidy generate
	CGO_ENABLED=0 GOOS=$(TARGET_GOOS) GOARCH=$(TARGET_GOARCH) \
	  go build -trimpath -ldflags="-s -w" -o $(BIN) $(CMD)
ifeq ($(TARGET_GOOS),linux)
	CGO_ENABLED=0 GOOS=$(TARGET_GOOS) GOARCH=$(TARGET_GOARCH) \
	  go build -trimpath -ldflags="-s -w" -o gocracker-vmm ./cmd/gocracker-vmm
	CGO_ENABLED=0 GOOS=$(TARGET_GOOS) GOARCH=$(TARGET_GOARCH) \
	  go build -trimpath -ldflags="-s -w" -o gocracker-jailer ./cmd/gocracker-jailer
endif

build-amd64:
	$(MAKE) build TARGET_GOARCH=amd64

build-arm64:
	$(MAKE) build TARGET_GOARCH=arm64

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
	rm -f $(BIN) internal/guest/init_amd64.bin internal/guest/init_arm64.bin
