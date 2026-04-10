BIN      := gocracker
MODULE   := github.com/gocracker/gocracker
CMD      := ./cmd/gocracker
TARGET_GOOS ?= $(shell go env GOOS)
TARGET_GOARCH ?= $(shell go env GOARCH)
PREFIX ?= /usr/local
DARWIN_ENTITLEMENTS ?= entitlements.local.plist
DARWIN_SIGN_IDENTITY ?= -
DARWIN_CODESIGN_FLAGS ?= -f

BINS_LINUX  := gocracker gocracker-vmm gocracker-jailer
BINS_DARWIN := gocracker gocracker-vmm gocracker-jailer

.PHONY: all build build-linux build-darwin build-darwin-e2e build-amd64 build-arm64 generate tidy test install clean kernel-host kernel-host-virtiofs kernel-guest kernel-guest-virtiofs hostcheck

all: build

## Pre-compile the guest init binary and embed it
generate:
	go generate ./internal/guest/

## Download dependencies, generate, and build the binary.
## On Linux: CGO_ENABLED=0 static binary.
## On Darwin: CGO_ENABLED=1 required for Virtualization.framework via vz.
## Local ad-hoc signing defaults to entitlements.local.plist because
## com.apple.vm.networking is a restricted entitlement and AMFI rejects
## ad-hoc binaries that carry it on this machine. Override
## DARWIN_ENTITLEMENTS=entitlements.plist when using a validated signing
## identity for the broader networking entitlement set. Override
## DARWIN_SIGN_IDENTITY and DARWIN_CODESIGN_FLAGS for Developer ID signing.
build: tidy generate
ifeq ($(TARGET_GOOS),darwin)
	@for bin in $(BINS_DARWIN); do \
		echo "building $$bin"; \
		CGO_ENABLED=1 GOOS=darwin GOARCH=$(TARGET_GOARCH) \
		  go build -trimpath -ldflags="-s -w" -o $$bin ./cmd/$$bin; \
		codesign --entitlements $(DARWIN_ENTITLEMENTS) $(DARWIN_CODESIGN_FLAGS) -s $(DARWIN_SIGN_IDENTITY) ./$$bin; \
	done
else
	@for bin in $(BINS_LINUX); do \
		echo "building $$bin"; \
		CGO_ENABLED=0 GOOS=$(TARGET_GOOS) GOARCH=$(TARGET_GOARCH) \
		  go build -trimpath -ldflags="-s -w" -o $$bin ./cmd/$$bin; \
	done
endif

build-linux:
	$(MAKE) build TARGET_GOOS=linux

build-darwin:
	$(MAKE) build TARGET_GOOS=darwin

build-darwin-e2e:
	$(MAKE) build TARGET_GOOS=darwin DARWIN_ENTITLEMENTS=entitlements.plist
	@./gocracker version >/dev/null 2>&1 || (echo "build-darwin-e2e: signed gocracker failed to launch. This host likely rejects ad-hoc com.apple.vm.networking binaries; set DARWIN_SIGN_IDENTITY to a real codesigning identity and rebuild." >&2; exit 1)
	@for helper in ./gocracker-vmm ./gocracker-jailer; do \
		$$helper >/dev/null 2>&1; status=$$?; \
		if [ $$status -ge 128 ]; then \
			echo "build-darwin-e2e: $$helper failed to launch after signing. This host likely rejects the signed helper binary." >&2; \
			exit 1; \
		fi; \
	done

build-amd64:
	$(MAKE) build TARGET_GOARCH=amd64

build-arm64:
	$(MAKE) build TARGET_GOARCH=arm64

## Install binaries to PREFIX/bin (default /usr/local/bin)
install: build
ifeq ($(TARGET_GOOS),darwin)
	@for bin in $(BINS_DARWIN); do \
		echo "installing $$bin → $(PREFIX)/bin/$$bin"; \
		install -m 755 ./$$bin $(PREFIX)/bin/$$bin; \
		codesign --entitlements $(DARWIN_ENTITLEMENTS) $(DARWIN_CODESIGN_FLAGS) -s $(DARWIN_SIGN_IDENTITY) $(PREFIX)/bin/$$bin; \
	done
else
	@for bin in $(BINS_LINUX); do \
		echo "installing $$bin → $(PREFIX)/bin/$$bin"; \
		install -m 755 ./$$bin $(PREFIX)/bin/$$bin; \
	done
endif

tidy:
	go mod tidy

test:
	go test ./...

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

## Run the API server on TCP port 8080 (easier for testing without root)
run: build
	./$(BIN) serve --addr :8080

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
	rm -f $(BINS_LINUX) $(BINS_DARWIN) internal/guest/init_amd64.bin internal/guest/init_arm64.bin
