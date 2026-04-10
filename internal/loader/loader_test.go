package loader

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"os"
	"testing"
)

// makeBzImageHeader builds a minimal valid bzImage header suitable for
// loadBzImage. The caller can override individual bytes afterwards.
func makeBzImageHeader(setupSectors byte, payloadLen int) []byte {
	// We need at least 0x250 bytes for the header area,
	// plus (setupSectors+1)*512 for the setup area,
	// plus the payload.
	setupBytes := (int(setupSectors) + 1) * 512
	total := setupBytes + payloadLen
	if total < 0x250 {
		total = 0x250 + payloadLen
	}
	buf := make([]byte, total)

	// Setup sectors at offset 0x1F1
	buf[setupSects] = setupSectors

	// Boot flag 0xAA55 at offset 0x1FE
	binary.LittleEndian.PutUint16(buf[bootFlag:], 0xAA55)

	// "HdrS" magic at offset 0x202 (0x53726448 little-endian)
	binary.LittleEndian.PutUint32(buf[headerMagic:], 0x53726448)

	// Protocol version 0x020F at offset 0x206
	binary.LittleEndian.PutUint16(buf[protocolVer:], 0x020F)

	return buf
}

func TestLoadKernel_UnknownFormat(t *testing.T) {
	mem := make([]byte, 4*1024*1024)
	// Write a small scratch file that is neither ELF nor bzImage
	tmp := t.TempDir()
	path := tmp + "/junk"
	data := []byte("this is not a kernel image at all")
	if err := writeFile(path, data); err != nil {
		t.Fatal(err)
	}
	_, err := LoadKernel(mem, path, 0x7000)
	if err == nil {
		t.Fatal("expected error for unknown kernel format, got nil")
	}
	if got := err.Error(); !contains(got, "unknown kernel format") {
		t.Fatalf("unexpected error: %s", got)
	}
}

func TestLoadKernel_FileMissing(t *testing.T) {
	mem := make([]byte, 4*1024*1024)
	_, err := LoadKernel(mem, "/nonexistent/kernel", 0x7000)
	if err == nil {
		t.Fatal("expected error for missing kernel, got nil")
	}
}

func TestLoadBzImage_BadMagic(t *testing.T) {
	// Valid boot flag but wrong HdrS magic
	buf := make([]byte, 0x300)
	binary.LittleEndian.PutUint16(buf[bootFlag:], 0xAA55)
	binary.LittleEndian.PutUint32(buf[headerMagic:], 0xDEADBEEF) // wrong magic
	binary.LittleEndian.PutUint16(buf[protocolVer:], 0x020F)
	buf[setupSects] = 4

	mem := make([]byte, 4*1024*1024)
	_, err := loadBzImage(mem, buf, 0x7000)
	if err == nil {
		t.Fatal("expected error for bad magic")
	}
	if got := err.Error(); !contains(got, "bad bzImage magic") {
		t.Fatalf("unexpected error: %s", got)
	}
}

func TestLoadBzImage_TooSmall(t *testing.T) {
	mem := make([]byte, 4*1024*1024)
	_, err := loadBzImage(mem, make([]byte, 100), 0x7000)
	if err == nil {
		t.Fatal("expected error for too-small image")
	}
}

func TestLoadBzImage_OldProtocol(t *testing.T) {
	buf := make([]byte, 0x300)
	binary.LittleEndian.PutUint16(buf[bootFlag:], 0xAA55)
	binary.LittleEndian.PutUint32(buf[headerMagic:], 0x53726448)
	binary.LittleEndian.PutUint16(buf[protocolVer:], 0x0100) // too old
	buf[setupSects] = 4

	mem := make([]byte, 4*1024*1024)
	_, err := loadBzImage(mem, buf, 0x7000)
	if err == nil {
		t.Fatal("expected error for old protocol")
	}
	if got := err.Error(); !contains(got, "boot protocol too old") {
		t.Fatalf("unexpected error: %s", got)
	}
}

func TestLoadBzImage_GzipPayload(t *testing.T) {
	// Create a gzip-compressed payload that will be the "kernel"
	payload := bytes.Repeat([]byte{0x90}, 256) // some NOP bytes
	var gzBuf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&gzBuf, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	gw.Write(payload)
	gw.Close()

	setupSecs := byte(4)
	hdr := makeBzImageHeader(setupSecs, gzBuf.Len())
	setupBytes := (int(setupSecs) + 1) * 512
	// Place the compressed data right after the setup area
	copy(hdr[setupBytes:], gzBuf.Bytes())

	mem := make([]byte, 4*1024*1024)
	info, err := loadBzImage(mem, hdr, 0x7000)
	if err != nil {
		t.Fatalf("loadBzImage: %v", err)
	}
	if info.EntryPoint != 0x100000 {
		t.Errorf("entry point = %#x, want %#x", info.EntryPoint, 0x100000)
	}
	if info.Protocol != 0x020F {
		t.Errorf("protocol = %#x, want 0x020F", info.Protocol)
	}
	// Verify the decompressed payload was placed at 0x100000
	for i := 0; i < len(payload); i++ {
		if mem[0x100000+i] != payload[i] {
			t.Fatalf("mismatch at offset %d in guest memory", i)
		}
	}
}

func TestLoadBzImage_SetupSectorsZero(t *testing.T) {
	// When setup_sects == 0, the loader should treat it as 4
	payload := []byte("rawpayload12345678901234567890123")
	hdr := makeBzImageHeader(4, len(payload))
	// Override the setup_sects field to 0 after header construction
	hdr[setupSects] = 0
	setupBytes := (4 + 1) * 512
	copy(hdr[setupBytes:], payload)

	mem := make([]byte, 4*1024*1024)
	info, err := loadBzImage(mem, hdr, 0x7000)
	if err != nil {
		t.Fatalf("loadBzImage with setup_sects=0: %v", err)
	}
	if info.EntryPoint != 0x100000 {
		t.Errorf("entry point = %#x, want %#x", info.EntryPoint, 0x100000)
	}
}

func TestDecompressKernelPayload_Gzip(t *testing.T) {
	original := []byte("hello, kernel world!")
	var buf bytes.Buffer
	gw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	gw.Write(original)
	gw.Close()

	out, err := decompressKernelPayload(buf.Bytes(), nil)
	if err != nil {
		t.Fatalf("decompressKernelPayload: %v", err)
	}
	if !bytes.Equal(out, original) {
		t.Fatalf("decompressed data mismatch: got %q, want %q", out, original)
	}
}

func TestNormalizeKernelImageBytes_ELFPassthrough(t *testing.T) {
	data := []byte{0x7F, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00}
	out, err := normalizeKernelImageBytes(data)
	if err != nil {
		t.Fatalf("normalizeKernelImageBytes: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatal("ELF image was not passed through verbatim")
	}
}

func TestExtractVmlinuxFromBzImage_GzipELFPayload(t *testing.T) {
	elfPayload := []byte{0x7F, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00, 'g', 'c'}
	var gzBuf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&gzBuf, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Write(elfPayload); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	setupSecs := byte(4)
	hdr := makeBzImageHeader(setupSecs, gzBuf.Len())
	setupBytes := (int(setupSecs) + 1) * 512
	copy(hdr[setupBytes:], gzBuf.Bytes())

	out, err := extractVmlinuxFromBzImage(hdr)
	if err != nil {
		t.Fatalf("extractVmlinuxFromBzImage: %v", err)
	}
	if !bytes.Equal(out, elfPayload) {
		t.Fatalf("extracted payload mismatch: got %x want %x", out, elfPayload)
	}
}

func TestDecompressKernelPayload_GzipWithStubPrefix(t *testing.T) {
	// Simulate a bzImage where there is a decompressor stub before
	// the gzip magic bytes
	original := []byte("decompressed kernel contents")
	var buf bytes.Buffer
	gw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	gw.Write(original)
	gw.Close()

	// Prepend some "stub" bytes that don't match any compression magic
	stub := bytes.Repeat([]byte{0x55}, 64)
	payload := append(stub, buf.Bytes()...)

	out, err := decompressKernelPayload(payload, nil)
	if err != nil {
		t.Fatalf("decompressKernelPayload: %v", err)
	}
	if !bytes.Equal(out, original) {
		t.Fatalf("data mismatch after stub+gzip: got len=%d, want len=%d", len(out), len(original))
	}
}

func TestDecompressKernelPayload_PassthroughUnrecognized(t *testing.T) {
	// Unrecognized data should be returned as-is
	data := []byte("some random unrecognized data bytes")
	out, err := decompressKernelPayload(data, nil)
	if err != nil {
		t.Fatalf("decompressKernelPayload: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatal("unrecognized data was not passed through verbatim")
	}
}

func makeARM64Image(payloadLen int, textOffset, imageSize uint64) []byte {
	if payloadLen < arm64ImageHeaderSize {
		payloadLen = arm64ImageHeaderSize
	}
	buf := make([]byte, payloadLen)
	binary.LittleEndian.PutUint64(buf[arm64ImageTextOffset:], textOffset)
	binary.LittleEndian.PutUint64(buf[arm64ImageSizeOffset:], imageSize)
	binary.LittleEndian.PutUint32(buf[arm64ImageMagicOffset:], arm64ImageMagic)
	for i := arm64ImageHeaderSize; i < len(buf); i++ {
		buf[i] = byte(i)
	}
	return buf
}

func TestLoadArm64Kernel_Image(t *testing.T) {
	mem := make([]byte, 16*1024*1024)
	image := makeARM64Image(4096, 0x80000, 4096)

	info, err := loadArm64KernelBytes(mem, image, 0x40000000)
	if err != nil {
		t.Fatalf("loadArm64KernelBytes: %v", err)
	}
	if info.EntryPoint != 0x40080000 {
		t.Fatalf("entry point = %#x, want %#x", info.EntryPoint, 0x40080000)
	}
	if info.KernelEnd != 0x40081000 {
		t.Fatalf("kernel end = %#x, want %#x", info.KernelEnd, 0x40081000)
	}
	got := mem[0x80000 : 0x80000+uint64(len(image))]
	if !bytes.Equal(got, image) {
		t.Fatal("arm64 image payload was not copied into guest RAM at text_offset")
	}
}

func TestLoadArm64Kernel_GzipImage(t *testing.T) {
	mem := make([]byte, 16*1024*1024)
	image := makeARM64Image(4096, 0x100000, 4096)
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(image); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := loadArm64KernelBytes(mem, compressed.Bytes(), 0x40000000)
	if err != nil {
		t.Fatalf("loadArm64KernelBytes gzip: %v", err)
	}
	if info.EntryPoint != 0x40100000 {
		t.Fatalf("entry point = %#x, want %#x", info.EntryPoint, 0x40100000)
	}
	got := mem[0x100000 : 0x100000+uint64(len(image))]
	if !bytes.Equal(got, image) {
		t.Fatal("arm64 gzip image payload was not copied after decompression")
	}
}

func TestLoadArm64Kernel_RejectsUnknownFormat(t *testing.T) {
	mem := make([]byte, 16*1024*1024)
	if _, err := loadArm64KernelBytes(mem, []byte("not-a-kernel"), 0x40000000); err == nil {
		t.Fatal("loadArm64KernelBytes() error = nil, want unknown format")
	}
}

func TestLoadArm64Kernel_RequiresAlignedGuestBase(t *testing.T) {
	mem := make([]byte, 16*1024*1024)
	image := makeARM64Image(4096, 0x80000, 4096)
	if _, err := loadArm64KernelBytes(mem, image, 0x40001000); err == nil {
		t.Fatal("loadArm64KernelBytes() error = nil, want alignment rejection")
	}
}

func TestDecompressKernelPayload_TooShort(t *testing.T) {
	// Data shorter than 4 bytes should be returned as-is
	data := []byte{0x01, 0x02}
	out, err := decompressKernelPayload(data, nil)
	if err != nil {
		t.Fatalf("decompressKernelPayload: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatal("short data was not passed through verbatim")
	}
}

func TestDecompressKernelPayload_ELFPassthrough(t *testing.T) {
	// ELF magic bytes should be returned as-is (no decompression)
	data := make([]byte, 64)
	data[0] = 0x7F
	data[1] = 'E'
	data[2] = 'L'
	data[3] = 'F'

	out, err := decompressKernelPayload(data, nil)
	if err != nil {
		t.Fatalf("decompressKernelPayload: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatal("ELF data was not passed through verbatim")
	}
}

func TestDecompressKernelPayload_PrecisePayload(t *testing.T) {
	// Simulate a bzImage where the compressed payload is past the 64KB scan limit
	// but we have precise payload from the setup header
	original := []byte("kernel contents from precise payload offset")
	var buf bytes.Buffer
	gw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	gw.Write(original)
	gw.Close()

	// Large stub > 64KB means scan would miss it
	stub := bytes.Repeat([]byte{0x55}, 70000)
	data := append(stub, buf.Bytes()...)

	// Without precise payload — scan misses it, returns data as-is
	out, err := decompressKernelPayload(data, nil)
	if err != nil {
		t.Fatalf("decompressKernelPayload: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatal("expected passthrough when magic is beyond scan limit")
	}

	// With precise payload — should decompress correctly
	out, err = decompressKernelPayload(data, buf.Bytes())
	if err != nil {
		t.Fatalf("decompressKernelPayload with precise: %v", err)
	}
	if !bytes.Equal(out, original) {
		t.Fatalf("precise payload decompress mismatch: got len=%d, want len=%d", len(out), len(original))
	}
}

func TestWriteBootParams(t *testing.T) {
	mem := make([]byte, 256*1024)
	info := &KernelInfo{
		EntryPoint: 0x100000,
		SetupBase:  0x7000,
		Protocol:   0x020F,
	}
	cfg := BootConfig{
		MemBytes:   128 * 1024 * 1024,
		Cmdline:    "console=ttyS0 quiet",
		InitrdAddr: 0x1000000,
		InitrdSize: 4096,
		ACPIRSDP:   0xE0000,
	}
	WriteBootParams(mem, info, cfg)

	// e820 entry count at base+0x1E8
	if mem[0x7000+0x1E8] != 4 {
		t.Errorf("e820 count = %d, want 4", mem[0x7000+0x1E8])
	}
	// Initrd address should be set
	gotInitrd := binary.LittleEndian.Uint32(mem[0x7000+ramdiskAddr:])
	if gotInitrd != 0x1000000 {
		t.Errorf("initrd addr = %#x, want %#x", gotInitrd, 0x1000000)
	}
	gotInitrdSz := binary.LittleEndian.Uint32(mem[0x7000+ramdiskSize:])
	if gotInitrdSz != 4096 {
		t.Errorf("initrd size = %d, want 4096", gotInitrdSz)
	}
	gotRSDP := binary.LittleEndian.Uint64(mem[0x7000+acpiRSDPAddr:])
	if gotRSDP != 0xE0000 {
		t.Errorf("acpi rsdp = %#x, want %#x", gotRSDP, uint64(0xE0000))
	}
}

func TestLoadELF(t *testing.T) {
	// Build a minimal 64-bit ELF with a single PT_LOAD segment
	// We construct the ELF header by hand so this test is self-contained.
	const loadAddr = 0x100000
	code := bytes.Repeat([]byte{0xCC}, 128) // payload

	// ELF header (64 bytes)
	var elf bytes.Buffer
	// e_ident
	elf.Write([]byte{0x7F, 'E', 'L', 'F'}) // magic
	elf.WriteByte(2)                       // 64-bit
	elf.WriteByte(1)                       // little-endian
	elf.WriteByte(1)                       // ELF version
	elf.WriteByte(0)                       // OS/ABI
	elf.Write(make([]byte, 8))             // padding
	// e_type (ET_EXEC=2)
	binary.Write(&elf, binary.LittleEndian, uint16(2))
	// e_machine (EM_X86_64=62)
	binary.Write(&elf, binary.LittleEndian, uint16(62))
	// e_version
	binary.Write(&elf, binary.LittleEndian, uint32(1))
	// e_entry
	binary.Write(&elf, binary.LittleEndian, uint64(loadAddr))
	// e_phoff (program header offset = 64, right after ELF header)
	binary.Write(&elf, binary.LittleEndian, uint64(64))
	// e_shoff
	binary.Write(&elf, binary.LittleEndian, uint64(0))
	// e_flags
	binary.Write(&elf, binary.LittleEndian, uint32(0))
	// e_ehsize
	binary.Write(&elf, binary.LittleEndian, uint16(64))
	// e_phentsize
	binary.Write(&elf, binary.LittleEndian, uint16(56))
	// e_phnum
	binary.Write(&elf, binary.LittleEndian, uint16(1))
	// e_shentsize
	binary.Write(&elf, binary.LittleEndian, uint16(0))
	// e_shnum
	binary.Write(&elf, binary.LittleEndian, uint16(0))
	// e_shstrndx
	binary.Write(&elf, binary.LittleEndian, uint16(0))

	// Program header (56 bytes for 64-bit)
	// p_type = PT_LOAD (1)
	binary.Write(&elf, binary.LittleEndian, uint32(1))
	// p_flags = PF_R|PF_X
	binary.Write(&elf, binary.LittleEndian, uint32(5))
	// p_offset — file offset of segment data
	dataOff := uint64(64 + 56)
	binary.Write(&elf, binary.LittleEndian, dataOff)
	// p_vaddr
	binary.Write(&elf, binary.LittleEndian, uint64(loadAddr))
	// p_paddr
	binary.Write(&elf, binary.LittleEndian, uint64(loadAddr))
	// p_filesz
	binary.Write(&elf, binary.LittleEndian, uint64(len(code)))
	// p_memsz
	binary.Write(&elf, binary.LittleEndian, uint64(len(code)))
	// p_align
	binary.Write(&elf, binary.LittleEndian, uint64(0x1000))

	// Segment data
	elf.Write(code)

	data := elf.Bytes()
	mem := make([]byte, 4*1024*1024)
	info, err := loadELF(mem, data)
	if err != nil {
		t.Fatalf("loadELF: %v", err)
	}
	if info.EntryPoint != loadAddr {
		t.Errorf("entry = %#x, want %#x", info.EntryPoint, loadAddr)
	}
	if info.KernelEnd != loadAddr+uint64(len(code)) {
		t.Errorf("kernelEnd = %#x, want %#x", info.KernelEnd, loadAddr+uint64(len(code)))
	}
	// Verify code was copied
	for i, b := range code {
		if mem[loadAddr+i] != b {
			t.Fatalf("mismatch at offset %d in guest memory", i)
		}
	}
}

func TestMatchCompression_TooShort(t *testing.T) {
	_, found := matchCompression([]byte{0x1F})
	if found {
		t.Fatal("matchCompression should return false for too-short data")
	}
}

// helpers

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0644)
}

func contains(s, substr string) bool {
	return bytes.Contains([]byte(s), []byte(substr))
}
