// Package loader handles loading Linux kernels (bzImage / ELF vmlinux)
// into guest physical memory and returns the entry point address.
package loader

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// bzImage setup header offsets (Documentation/x86/boot.rst)
const (
	setupSects        = 0x1F1
	bootFlag          = 0x1FE // must be 0xAA55
	headerMagic       = 0x202 // "HdrS"
	protocolVer       = 0x206
	realModeSwitch    = 0x210
	startSys          = 0x214
	kernelVersion     = 0x20E
	typeOfLoader      = 0x210
	loadFlags         = 0x211
	setupMoveSize     = 0x212
	code32Start       = 0x214
	ramdiskAddr       = 0x218
	ramdiskSize       = 0x21C
	heapEndPtr        = 0x224
	extLoaderVer      = 0x226
	extLoaderType     = 0x227
	cmdLinePtr        = 0x228
	kernelAlignment   = 0x230
	relocatableKernel = 0x234
	minAlignment      = 0x235
	xloadFlags        = 0x236
	cmdLineSize       = 0x238
	initrdAddrMax     = 0x22C
	payloadOffset     = 0x248 // protocol >= 0x0208
	payloadLength     = 0x24C // protocol >= 0x0208
	acpiRSDPAddr      = 0x070 // boot_params.acpi_rsdp_addr

	// loadFlags bits
	flagLoadHigh   = 0x01
	flagCanUseHeap = 0x80
)

// KernelInfo holds the result of loading a kernel image.
type KernelInfo struct {
	EntryPoint uint64 // guest physical address to jump to
	SetupBase  uint64 // guest physical address of boot_params / zero page
	KernelEnd  uint64 // first free byte after kernel in guest RAM
	Protocol   uint16 // boot protocol version
}

// LoadKernel loads a bzImage or ELF vmlinux into guest memory.
// bootParamsAddr is the guest-physical address where boot_params will live.
func LoadKernel(mem []byte, kernelPath string, bootParamsAddr uint64) (*KernelInfo, error) {
	f, err := os.Open(kernelPath)
	if err != nil {
		return nil, fmt.Errorf("open kernel %s: %w", kernelPath, err)
	}
	defer f.Close()

	var magic [0x200]byte
	n, _ := io.ReadFull(f, magic[:])

	if n >= 4 && magic[0] == 0x7F && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F' {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return loadELFFromFile(mem, f)
	}

	if n >= 0x200 && binary.LittleEndian.Uint16(magic[0x1FE:]) == 0xAA55 {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, fmt.Errorf("read kernel %s: %w", kernelPath, err)
		}
		// Try to normalize bzImage to ELF payload; fall back to legacy path.
		if vmlinux, err := extractVmlinuxFromBzImage(data); err == nil {
			if info, elfErr := loadELF(mem, vmlinux); elfErr == nil {
				return info, nil
			}
		}
		return loadBzImage(mem, data, bootParamsAddr)
	}

	return nil, fmt.Errorf("unknown kernel format (not ELF or bzImage)")
}

// loadELFFromFile loads an ELF vmlinux from an open file into guest RAM.
func loadELFFromFile(mem []byte, f *os.File) (*KernelInfo, error) {
	ef, err := elf.NewFile(f)
	if err != nil {
		return nil, fmt.Errorf("elf parse: %w", err)
	}
	defer ef.Close()

	type loadSeg struct {
		prog  *elf.Prog
		start uint64
		end   uint64
	}
	var segs []loadSeg
	for _, ph := range ef.Progs {
		if ph.Type != elf.PT_LOAD {
			continue
		}
		if ph.Memsz > math.MaxUint64-ph.Paddr || ph.Paddr+ph.Memsz > uint64(len(mem)) {
			return nil, fmt.Errorf("ELF segment [%#x,%#x) exceeds guest RAM",
				ph.Paddr, ph.Paddr+ph.Memsz)
		}
		if ph.Filesz > uint64(math.MaxInt) {
			return nil, fmt.Errorf("ELF segment filesz %#x exceeds addressable range", ph.Filesz)
		}
		if ph.Filesz > math.MaxUint64-ph.Paddr || ph.Paddr+ph.Filesz > uint64(len(mem)) {
			return nil, fmt.Errorf("ELF segment [%#x,%#x) filesz exceeds guest RAM",
				ph.Paddr, ph.Paddr+ph.Filesz)
		}
		segs = append(segs, loadSeg{prog: ph, start: ph.Paddr, end: ph.Paddr + ph.Memsz})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].start < segs[j].start })
	for i := 1; i < len(segs); i++ {
		if segs[i].start < segs[i-1].end {
			return nil, fmt.Errorf("ELF segments overlap: [%#x,%#x) and [%#x,%#x)",
				segs[i-1].start, segs[i-1].end, segs[i].start, segs[i].end)
		}
	}

	var kernEnd uint64
	var wg sync.WaitGroup
	errCh := make(chan error, len(segs))

	for _, s := range segs {
		wg.Add(1)
		go func(s loadSeg) {
			defer wg.Done()
			dst := mem[s.start : s.start+s.prog.Filesz]
			if _, err := s.prog.ReadAt(dst, 0); err != nil {
				errCh <- fmt.Errorf("read ELF segment: %w", err)
				return
			}
		}(s)
		if s.end > kernEnd {
			kernEnd = s.end
		}
	}
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		return nil, err
	}

	return &KernelInfo{
		EntryPoint: ef.Entry,
		SetupBase:  0x7000,
		KernelEnd:  kernEnd,
		Protocol:   0x020F,
	}, nil
}

// NormalizeKernelImage reads a kernel image and returns the preferred runtime
// representation for gocracker. ELF vmlinux is returned unchanged; bzImage
// inputs are normalized to their embedded ELF payload when possible.
func NormalizeKernelImage(kernelPath string) ([]byte, error) {
	data, err := os.ReadFile(kernelPath)
	if err != nil {
		return nil, fmt.Errorf("read kernel %s: %w", kernelPath, err)
	}
	return normalizeKernelImageBytes(data)
}

func normalizeKernelImageBytes(data []byte) ([]byte, error) {
	if len(data) >= 4 && data[0] == 0x7F && data[1] == 'E' && data[2] == 'L' && data[3] == 'F' {
		return data, nil
	}
	if len(data) > 0x200 && binary.LittleEndian.Uint16(data[bootFlag:]) == 0xAA55 {
		return extractVmlinuxFromBzImage(data)
	}
	return nil, fmt.Errorf("unknown kernel format (not ELF or bzImage)")
}

func extractVmlinuxFromBzImage(data []byte) ([]byte, error) {
	if len(data) < 0x250 {
		return nil, fmt.Errorf("bzImage too small")
	}
	if binary.LittleEndian.Uint32(data[headerMagic:]) != 0x53726448 {
		return nil, fmt.Errorf("bad bzImage magic")
	}

	protocol := binary.LittleEndian.Uint16(data[protocolVer:])
	if protocol < 0x0200 {
		return nil, fmt.Errorf("boot protocol too old: %#x", protocol)
	}

	setupSz := int(data[setupSects])
	if setupSz == 0 {
		setupSz = 4
	}
	setupBytes := (setupSz + 1) * 512
	if setupBytes >= len(data) {
		return nil, fmt.Errorf("setup sector overflows image")
	}

	pmKernel := data[setupBytes:]

	var precisePayload []byte
	if protocol >= 0x0208 && len(data) > payloadLength+4 {
		pOff := int(binary.LittleEndian.Uint32(data[payloadOffset:]))
		pLen := int(binary.LittleEndian.Uint32(data[payloadLength:]))
		if pOff > 0 && pLen > 0 && pOff+pLen <= len(pmKernel) {
			precisePayload = pmKernel[pOff : pOff+pLen]
		}
	}

	decompressed, err := decompressKernelPayload(pmKernel, precisePayload)
	if err != nil {
		return nil, fmt.Errorf("decompress kernel: %w", err)
	}
	if len(decompressed) < 4 || decompressed[0] != 0x7F || decompressed[1] != 'E' || decompressed[2] != 'L' || decompressed[3] != 'F' {
		return nil, fmt.Errorf("bzImage payload did not decompress to ELF vmlinux")
	}
	return decompressed, nil
}

// loadBzImage loads a compressed bzImage kernel.
func loadBzImage(mem, data []byte, bootParamsAddr uint64) (*KernelInfo, error) {
	if len(data) < 0x250 {
		return nil, fmt.Errorf("bzImage too small")
	}

	// Validate header
	if binary.LittleEndian.Uint32(data[headerMagic:]) != 0x53726448 {
		return nil, fmt.Errorf("bad bzImage magic")
	}

	protocol := binary.LittleEndian.Uint16(data[protocolVer:])
	if protocol < 0x0200 {
		return nil, fmt.Errorf("boot protocol too old: %#x", protocol)
	}

	// Calculate where the protected-mode kernel starts
	setupSz := int(data[setupSects])
	if setupSz == 0 {
		setupSz = 4
	}
	setupBytes := (setupSz + 1) * 512
	if setupBytes >= len(data) {
		return nil, fmt.Errorf("setup sector overflows image")
	}

	// Protected-mode kernel payload (compressed)
	pmKernel := data[setupBytes:]

	// Standard bzImage high-load address (1 MiB)
	const kernelLoadAddr = 0x100000

	// If protocol >= 2.08, use payload_offset to locate compressed data precisely.
	// payload_offset is relative to the start of the protected-mode code (pmKernel).
	var compressedPayload []byte
	if protocol >= 0x0208 && len(data) > payloadLength+4 {
		pOff := int(binary.LittleEndian.Uint32(data[payloadOffset:]))
		pLen := int(binary.LittleEndian.Uint32(data[payloadLength:]))
		if pOff > 0 && pLen > 0 && pOff+pLen <= len(pmKernel) {
			compressedPayload = pmKernel[pOff : pOff+pLen]
		}
	}

	decompressed, err := decompressKernelPayload(pmKernel, compressedPayload)
	if err != nil {
		return nil, fmt.Errorf("decompress kernel: %w", err)
	}

	if uint64(kernelLoadAddr)+uint64(len(decompressed)) > uint64(len(mem)) {
		return nil, fmt.Errorf("decompressed kernel too large for guest RAM (%d MiB needed)",
			(kernelLoadAddr+len(decompressed))/(1024*1024)+1)
	}

	copy(mem[kernelLoadAddr:], decompressed)

	// Copy boot params (zero-page) to bootParamsAddr
	// The first 512 bytes of the image is the real-mode code / boot sector
	// which also contains the setup header at offset 0x1F1
	if bootParamsAddr+4096 <= uint64(len(mem)) {
		copy(mem[bootParamsAddr:bootParamsAddr+512], data[:512])
	}

	// Set typeOfLoader = 0xFF (unknown bootloader), required by kernel
	mem[bootParamsAddr+typeOfLoader] = 0xFF

	// Enable heap
	if data[loadFlags]&flagCanUseHeap != 0 {
		mem[bootParamsAddr+loadFlags] |= flagCanUseHeap
		binary.LittleEndian.PutUint16(mem[bootParamsAddr+heapEndPtr:], 0xFE00)
	}

	info := &KernelInfo{
		EntryPoint: kernelLoadAddr,
		SetupBase:  bootParamsAddr,
		KernelEnd:  uint64(kernelLoadAddr + len(decompressed)),
		Protocol:   protocol,
	}
	return info, nil
}

// loadELF loads an uncompressed ELF vmlinux.
func loadELF(mem, data []byte) (*KernelInfo, error) {
	ef, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("elf parse: %w", err)
	}
	defer ef.Close()

	// Collect loadable segments, validate bounds, and check for overlap
	// before parallelizing I/O (concurrent writes to overlapping mem
	// regions would be a data race).
	type loadSeg struct {
		prog  *elf.Prog
		start uint64
		end   uint64 // paddr + memsz
	}
	var segs []loadSeg
	for _, ph := range ef.Progs {
		if ph.Type != elf.PT_LOAD {
			continue
		}
		if ph.Memsz > math.MaxUint64-ph.Paddr || ph.Paddr+ph.Memsz > uint64(len(mem)) {
			return nil, fmt.Errorf("ELF segment [%#x,%#x) exceeds guest RAM",
				ph.Paddr, ph.Paddr+ph.Memsz)
		}
		if ph.Filesz > uint64(math.MaxInt) {
			return nil, fmt.Errorf("ELF segment filesz %#x exceeds addressable range", ph.Filesz)
		}
		if ph.Filesz > math.MaxUint64-ph.Paddr || ph.Paddr+ph.Filesz > uint64(len(mem)) {
			return nil, fmt.Errorf("ELF segment [%#x,%#x) filesz exceeds guest RAM",
				ph.Paddr, ph.Paddr+ph.Filesz)
		}
		segs = append(segs, loadSeg{prog: ph, start: ph.Paddr, end: ph.Paddr + ph.Memsz})
	}
	// Check for overlapping segments (sort by start address).
	sort.Slice(segs, func(i, j int) bool { return segs[i].start < segs[j].start })
	for i := 1; i < len(segs); i++ {
		if segs[i].start < segs[i-1].end {
			return nil, fmt.Errorf("ELF segments overlap: [%#x,%#x) and [%#x,%#x)",
				segs[i-1].start, segs[i-1].end, segs[i].start, segs[i].end)
		}
	}

	var kernEnd uint64
	var wg sync.WaitGroup
	errCh := make(chan error, len(segs))

	for _, s := range segs {
		wg.Add(1)
		go func(s loadSeg) {
			defer wg.Done()
			dst := mem[s.start : s.start+s.prog.Filesz]
			if _, err := s.prog.ReadAt(dst, 0); err != nil {
				errCh <- fmt.Errorf("read ELF segment: %w", err)
				return
			}
		}(s)
		if s.end > kernEnd {
			kernEnd = s.end
		}
	}
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		return nil, err
	}

	return &KernelInfo{
		EntryPoint: ef.Entry,
		SetupBase:  0x7000,
		KernelEnd:  kernEnd,
		Protocol:   0x020F,
	}, nil
}

// WriteBootParams fills in Linux boot_params (zero-page) for the guest.
// Call after LoadKernel.
func WriteBootParams(mem []byte, info *KernelInfo, cfg BootConfig) {
	base := info.SetupBase

	// e820 memory map
	writeE820Map(mem, base, cfg.MemBytes)

	// Note: cmdline pointer and size are set by the caller (vmm.go)
	// to avoid double-writes and conflicting addresses.

	// Initrd
	if cfg.InitrdAddr != 0 && cfg.InitrdSize != 0 {
		binary.LittleEndian.PutUint32(mem[base+ramdiskAddr:], uint32(cfg.InitrdAddr))
		binary.LittleEndian.PutUint32(mem[base+ramdiskSize:], uint32(cfg.InitrdSize))
	}
	if cfg.ACPIRSDP != 0 {
		binary.LittleEndian.PutUint64(mem[base+acpiRSDPAddr:], cfg.ACPIRSDP)
	}

	// vid_mode = 0xFFFF (normal)
	binary.LittleEndian.PutUint16(mem[base+0x1FA:], 0xFFFF)
}

// BootConfig holds parameters for WriteBootParams.
type BootConfig struct {
	MemBytes   uint64
	Cmdline    string
	InitrdAddr uint64
	InitrdSize uint64
	ACPIRSDP   uint64
}

// decompressKernelPayload detects compression format by magic bytes and decompresses.
// Supports gzip, bzip2, xz, lzma, lz4, zstd. Returns data as-is if ELF or unknown.
// If precisePayload is non-nil, try it first (from setup header payload_offset).
func decompressKernelPayload(data []byte, precisePayload []byte) ([]byte, error) {
	if len(data) < 4 {
		return data, nil
	}

	// Try precise payload first (from bzImage setup header payload_offset/payload_length)
	if len(precisePayload) > 4 {
		if out := tryDecompress(precisePayload); out != nil {
			return out, nil
		}
	}

	// Fallback: scan for compression magic within the first 64KB of the payload
	scanLimit := 65536
	if scanLimit > len(data)-4 {
		scanLimit = len(data) - 4
	}
	for off := 0; off < scanLimit; off++ {
		r, found := matchCompression(data[off:])
		if !found {
			continue
		}
		out, _ := io.ReadAll(r)
		if rc, ok := r.(io.Closer); ok {
			rc.Close()
		}
		if len(out) > 0 {
			return out, nil
		}
	}

	// ELF or unrecognized — return as-is
	return data, nil
}

// tryDecompress attempts to decompress data. Returns nil on failure.
func tryDecompress(data []byte) []byte {
	r, found := matchCompression(data)
	if !found {
		return nil
	}
	out, err := io.ReadAll(r)
	if rc, ok := r.(io.Closer); ok {
		rc.Close()
	}
	// Accept partial output: decompressors may error on trailing data
	// after the compressed stream (e.g. zstd hitting non-frame bytes).
	if len(out) > 0 {
		return out
	}
	if err != nil {
		return nil
	}
	return nil
}

func matchCompression(data []byte) (io.Reader, bool) {
	if len(data) < 4 {
		return nil, false
	}
	switch {
	// gzip: 0x1F 0x8B
	case data[0] == 0x1F && data[1] == 0x8B:
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, false
		}
		return r, true

	// bzip2: 0x42 0x5A 0x68 ("BZh")
	case data[0] == 0x42 && data[1] == 0x5A && data[2] == 0x68:
		return bzip2.NewReader(bytes.NewReader(data)), true

	// xz: 0xFD 0x37 0x7A 0x58 0x5A 0x00
	case len(data) >= 6 && data[0] == 0xFD && data[1] == 0x37 && data[2] == 0x7A &&
		data[3] == 0x58 && data[4] == 0x5A && data[5] == 0x00:
		r, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, false
		}
		return r, true

	// lzma: 0x5D 0x00
	case data[0] == 0x5D && data[1] == 0x00:
		r, err := lzma.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, false
		}
		return r, true

	// lz4 legacy (Linux kernel format): 0x02 0x21 0x4C 0x18
	case len(data) >= 4 && data[0] == 0x02 && data[1] == 0x21 && data[2] == 0x4C && data[3] == 0x18:
		return lz4.NewReader(bytes.NewReader(data)), true

	// zstd: 0x28 0xB5 0x2F 0xFD
	case data[0] == 0x28 && data[1] == 0xB5 && data[2] == 0x2F && data[3] == 0xFD:
		r, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, false
		}
		return r, true
	}
	return nil, false
}

func writeE820Map(mem []byte, base uint64, totalMem uint64) {
	// e820 entry count at offset 0x1E8
	mem[base+0x1E8] = 4

	e820 := base + 0x2D0
	// [0x00000000 - 0x0009FFFF] conventional RAM (640 KiB)
	putE820(mem, e820+0, 0x00000000, 0x0009FC00, 1)
	// [0x0009FC00 - 0x000DFFFF] reserved for low-memory firmware structures
	// including MP tables / ACPI tables written by the VMM.
	putE820(mem, e820+20, 0x0009FC00, 0x00040400, 2)
	// [0x000E0000 - 0x000FFFFF] BIOS ROM (reserved)
	putE820(mem, e820+40, 0x000E0000, 0x00020000, 2)
	// [0x00100000 - end] extended RAM
	if totalMem > 0x00100000 {
		putE820(mem, e820+60, 0x00100000, totalMem-0x00100000, 1)
	}
}

func putE820(mem []byte, off, addr, size, typ uint64) {
	binary.LittleEndian.PutUint64(mem[off:], addr)
	binary.LittleEndian.PutUint64(mem[off+8:], size)
	binary.LittleEndian.PutUint32(mem[off+16:], uint32(typ))
}
