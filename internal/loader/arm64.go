package loader

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

const (
	arm64ImageHeaderSize  = 64
	arm64ImageMagicOffset = 56
	arm64ImageTextOffset  = 8
	arm64ImageSizeOffset  = 16
	arm64ImageMagic       = 0x644d5241 // "ARM\x64"
	arm64ImageDefaultText = 0x80000
	arm64ImageAlign       = 0x200000
)

// LoadArm64Kernel loads an arm64 Image/Image.gz or ELF vmlinux into guest RAM.
// guestMemBase is the physical base address covered by mem.
func LoadArm64Kernel(mem []byte, kernelPath string, guestMemBase uint64) (*KernelInfo, error) {
	f, err := os.Open(kernelPath)
	if err != nil {
		return nil, fmt.Errorf("open kernel %s: %w", kernelPath, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat kernel %s: %w", kernelPath, err)
	}
	size := fi.Size()

	// Read the header to determine format.
	var header [arm64ImageHeaderSize]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return nil, fmt.Errorf("read kernel header %s: %w", kernelPath, err)
	}

	// ELF: fall back to full read (needs elf.NewFile).
	if header[0] == 0x7F && header[1] == 'E' && header[2] == 'L' && header[3] == 'F' {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, fmt.Errorf("read kernel %s: %w", kernelPath, err)
		}
		return loadELF(mem, data)
	}

	// Compressed Image or unknown: fall back to the compatibility path.
	if !looksLikeARM64Image(header[:]) {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, fmt.Errorf("read kernel %s: %w", kernelPath, err)
		}
		return loadArm64KernelBytes(mem, data, guestMemBase)
	}

	// Uncompressed arm64 Image: read directly into guest RAM.
	textOffset := binary.LittleEndian.Uint64(header[arm64ImageTextOffset:])
	imageSize := binary.LittleEndian.Uint64(header[arm64ImageSizeOffset:])
	fileSize := uint64(size)
	if imageSize == 0 {
		imageSize = fileSize
		if textOffset == 0 {
			textOffset = arm64ImageDefaultText
		}
	}
	if guestMemBase%arm64ImageAlign != 0 {
		return nil, fmt.Errorf("arm64 guest memory base %#x must be 2MiB-aligned", guestMemBase)
	}

	loadAddr := guestMemBase + textOffset
	loadOffset := loadAddr - guestMemBase
	if loadOffset+fileSize > uint64(len(mem)) {
		return nil, fmt.Errorf("arm64 kernel image at %#x (%d bytes) exceeds guest RAM", loadAddr, fileSize)
	}

	// Read directly from the file into guest RAM to keep the uncompressed Image
	// path allocation-free and predictable.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(f, mem[loadOffset:loadOffset+fileSize]); err != nil {
		return nil, fmt.Errorf("read kernel into guest RAM: %w", err)
	}

	kernelEnd := loadAddr + imageSize
	if imageSize < fileSize {
		kernelEnd = loadAddr + fileSize
	}
	return &KernelInfo{
		EntryPoint: loadAddr,
		KernelEnd:  kernelEnd,
		Protocol:   0,
	}, nil
}

func loadArm64KernelBytes(mem, data []byte, guestMemBase uint64) (*KernelInfo, error) {
	if len(data) >= 4 && data[0] == 0x7F && data[1] == 'E' && data[2] == 'L' && data[3] == 'F' {
		return loadELF(mem, data)
	}

	payload, err := decompressKernelPayload(data, nil)
	if err != nil {
		return nil, fmt.Errorf("decompress arm64 kernel: %w", err)
	}
	if !looksLikeARM64Image(payload) {
		return nil, fmt.Errorf("unknown arm64 kernel format (expected ELF, Image, or Image.gz)")
	}
	if guestMemBase%arm64ImageAlign != 0 {
		return nil, fmt.Errorf("arm64 guest memory base %#x must be 2MiB-aligned", guestMemBase)
	}

	textOffset := binary.LittleEndian.Uint64(payload[arm64ImageTextOffset:])
	imageSize := binary.LittleEndian.Uint64(payload[arm64ImageSizeOffset:])
	if imageSize == 0 {
		imageSize = uint64(len(payload))
		if textOffset == 0 {
			textOffset = arm64ImageDefaultText
		}
	}

	loadAddr := guestMemBase + textOffset
	if loadAddr < guestMemBase {
		return nil, fmt.Errorf("arm64 kernel load address overflow")
	}
	loadOffset := loadAddr - guestMemBase
	if loadOffset > uint64(len(mem)) {
		return nil, fmt.Errorf("arm64 kernel load offset %#x exceeds guest RAM window", loadOffset)
	}
	if loadOffset+uint64(len(payload)) > uint64(len(mem)) {
		return nil, fmt.Errorf("arm64 kernel image at %#x (%d bytes) exceeds guest RAM", loadAddr, len(payload))
	}

	copy(mem[loadOffset:loadOffset+uint64(len(payload))], payload)

	kernelEnd := loadAddr + imageSize
	if imageSize < uint64(len(payload)) {
		kernelEnd = loadAddr + uint64(len(payload))
	}

	return &KernelInfo{
		EntryPoint: loadAddr,
		KernelEnd:  kernelEnd,
		Protocol:   0,
	}, nil
}

func looksLikeARM64Image(data []byte) bool {
	return len(data) >= arm64ImageHeaderSize &&
		binary.LittleEndian.Uint32(data[arm64ImageMagicOffset:]) == arm64ImageMagic
}
