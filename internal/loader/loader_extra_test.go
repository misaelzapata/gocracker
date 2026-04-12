package loader

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestMatchCompressionGzip(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Write([]byte("hello"))
	gw.Close()
	r, found := matchCompression(buf.Bytes())
	if !found {
		t.Fatal("should match gzip")
	}
	if r == nil {
		t.Fatal("reader should not be nil")
	}
}

func TestMatchCompressionBzip2(t *testing.T) {
	// bzip2 magic: 0x42 0x5A 0x68
	data := []byte{0x42, 0x5A, 0x68, 0x39, 0x00, 0x00} // minimal bzip2-like
	r, found := matchCompression(data)
	if !found {
		t.Fatal("should match bzip2")
	}
	if r == nil {
		t.Fatal("reader should not be nil")
	}
}

func TestMatchCompressionZstd(t *testing.T) {
	var buf bytes.Buffer
	w, _ := zstd.NewWriter(&buf)
	w.Write([]byte("hello"))
	w.Close()
	r, found := matchCompression(buf.Bytes())
	if !found {
		t.Fatal("should match zstd")
	}
	if r == nil {
		t.Fatal("reader should not be nil")
	}
}

func TestMatchCompressionLZ4Legacy(t *testing.T) {
	// LZ4 legacy magic: 0x02 0x21 0x4C 0x18
	data := []byte{0x02, 0x21, 0x4C, 0x18, 0x00, 0x00, 0x00, 0x00}
	r, found := matchCompression(data)
	if !found {
		t.Fatal("should match lz4")
	}
	if r == nil {
		t.Fatal("reader should not be nil")
	}
}

func TestMatchCompressionUnknown(t *testing.T) {
	_, found := matchCompression([]byte{0x00, 0x01, 0x02, 0x03})
	if found {
		t.Fatal("should not match unknown magic")
	}
}

func TestTryDecompressGzip(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Write([]byte("kernel data"))
	gw.Close()
	out := tryDecompress(buf.Bytes())
	if string(out) != "kernel data" {
		t.Fatalf("tryDecompress(gzip) = %q", out)
	}
}

func TestTryDecompressUnrecognized(t *testing.T) {
	out := tryDecompress([]byte{0x00, 0x01, 0x02, 0x03})
	if out != nil {
		t.Fatalf("tryDecompress(unknown) = %v, want nil", out)
	}
}

func TestTryDecompressTooShort(t *testing.T) {
	out := tryDecompress([]byte{0x01})
	if out != nil {
		t.Fatalf("tryDecompress(short) = %v, want nil", out)
	}
}

func TestNormalizeKernelImageBytesUnknown(t *testing.T) {
	_, err := normalizeKernelImageBytes([]byte("not a kernel"))
	if err == nil {
		t.Fatal("expected error for unknown format")
	}
}

func TestNormalizeKernelImageBytesBzImage(t *testing.T) {
	elfPayload := []byte{0x7F, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00, 0x01, 0x02}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	gw.Write(elfPayload)
	gw.Close()

	setupSecs := byte(4)
	hdr := makeBzImageHeader(setupSecs, gzBuf.Len())
	setupBytes := (int(setupSecs) + 1) * 512
	copy(hdr[setupBytes:], gzBuf.Bytes())

	out, err := normalizeKernelImageBytes(hdr)
	if err != nil {
		t.Fatalf("normalizeKernelImageBytes: %v", err)
	}
	if !bytes.Equal(out, elfPayload) {
		t.Fatal("payload mismatch")
	}
}

func TestExtractVmlinuxFromBzImageTooSmall(t *testing.T) {
	_, err := extractVmlinuxFromBzImage(make([]byte, 100))
	if err == nil {
		t.Fatal("expected error for too-small image")
	}
}

func TestExtractVmlinuxFromBzImageBadMagic(t *testing.T) {
	buf := make([]byte, 0x300)
	binary.LittleEndian.PutUint16(buf[bootFlag:], 0xAA55)
	binary.LittleEndian.PutUint32(buf[headerMagic:], 0xDEADBEEF)
	_, err := extractVmlinuxFromBzImage(buf)
	if err == nil {
		t.Fatal("expected error for bad magic")
	}
}

func TestExtractVmlinuxFromBzImageOldProtocol(t *testing.T) {
	buf := make([]byte, 0x300)
	binary.LittleEndian.PutUint16(buf[bootFlag:], 0xAA55)
	binary.LittleEndian.PutUint32(buf[headerMagic:], 0x53726448)
	binary.LittleEndian.PutUint16(buf[protocolVer:], 0x0100)
	_, err := extractVmlinuxFromBzImage(buf)
	if err == nil {
		t.Fatal("expected error for old protocol")
	}
}

func TestWriteBootParamsNoInitrd(t *testing.T) {
	mem := make([]byte, 256*1024)
	info := &KernelInfo{
		EntryPoint: 0x100000,
		SetupBase:  0x7000,
		Protocol:   0x020F,
	}
	cfg := BootConfig{
		MemBytes: 128 * 1024 * 1024,
	}
	WriteBootParams(mem, info, cfg)
	// e820 should still be set
	if mem[0x7000+0x1E8] != 4 {
		t.Errorf("e820 count = %d, want 4", mem[0x7000+0x1E8])
	}
	// Initrd should be 0
	gotInitrd := binary.LittleEndian.Uint32(mem[0x7000+ramdiskAddr:])
	if gotInitrd != 0 {
		t.Errorf("initrd addr = %#x, want 0", gotInitrd)
	}
}

func TestWriteBootParamsSmallMem(t *testing.T) {
	mem := make([]byte, 256*1024)
	info := &KernelInfo{SetupBase: 0x7000}
	cfg := BootConfig{MemBytes: 0x80000} // less than 1MB
	WriteBootParams(mem, info, cfg)
	// e820 should still work, just with smaller extended RAM
	if mem[0x7000+0x1E8] != 4 {
		t.Errorf("e820 count = %d, want 4", mem[0x7000+0x1E8])
	}
}

func TestDecompressKernelPayloadZstd(t *testing.T) {
	original := []byte("zstd kernel payload")
	var buf bytes.Buffer
	w, _ := zstd.NewWriter(&buf)
	w.Write(original)
	w.Close()

	out, err := decompressKernelPayload(buf.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, original) {
		t.Fatalf("decompressed = %q, want %q", out, original)
	}
}

func TestLoadBzImageSetupSectorOverflow(t *testing.T) {
	buf := make([]byte, 0x300)
	binary.LittleEndian.PutUint16(buf[bootFlag:], 0xAA55)
	binary.LittleEndian.PutUint32(buf[headerMagic:], 0x53726448)
	binary.LittleEndian.PutUint16(buf[protocolVer:], 0x020F)
	buf[setupSects] = 200 // would overflow

	mem := make([]byte, 4*1024*1024)
	_, err := loadBzImage(mem, buf, 0x7000)
	if err == nil {
		t.Fatal("expected error for setup sector overflow")
	}
}

func TestLoadBzImageDecompressedTooLarge(t *testing.T) {
	// Create a valid bzImage with payload that decompresses to more than guest RAM
	// Use a small guest memory
	payload := bytes.Repeat([]byte{0x90}, 256)
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	gw.Write(payload)
	gw.Close()

	setupSecs := byte(4)
	hdr := makeBzImageHeader(setupSecs, gzBuf.Len())
	setupBytes := (int(setupSecs) + 1) * 512
	copy(hdr[setupBytes:], gzBuf.Bytes())

	// Tiny guest memory
	mem := make([]byte, 0x100000) // exactly 1MB, not enough for 0x100000 + 256
	_, err := loadBzImage(mem, hdr, 0x7000)
	if err == nil {
		t.Fatal("expected error for decompressed kernel too large")
	}
}

func TestLoadKernelBzImageWithELFPayload(t *testing.T) {
	// Create a bzImage that contains a gzip'd ELF as payload
	// The ELF should be loaded via the ELF path
	code := bytes.Repeat([]byte{0xCC}, 128)
	elfData := makeMinimalELF(0x100000, code)
	
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	gw.Write(elfData)
	gw.Close()

	setupSecs := byte(4)
	hdr := makeBzImageHeader(setupSecs, gzBuf.Len())
	setupBytes := (int(setupSecs) + 1) * 512
	copy(hdr[setupBytes:], gzBuf.Bytes())

	tmp := t.TempDir()
	path := tmp + "/bzImage"
	if err := writeFile(path, hdr); err != nil {
		t.Fatal(err)
	}

	mem := make([]byte, 4*1024*1024)
	info, err := LoadKernel(mem, path, 0x7000)
	if err != nil {
		t.Fatalf("LoadKernel: %v", err)
	}
	if info.EntryPoint != 0x100000 {
		t.Fatalf("entry = %#x, want %#x", info.EntryPoint, 0x100000)
	}
}

// makeMinimalELF builds a minimal 64-bit ELF binary with one PT_LOAD segment.
func makeMinimalELF(loadAddr uint64, code []byte) []byte {
	var elf bytes.Buffer
	elf.Write([]byte{0x7F, 'E', 'L', 'F'})
	elf.WriteByte(2) // 64-bit
	elf.WriteByte(1) // little-endian
	elf.WriteByte(1) // ELF version
	elf.WriteByte(0) // OS/ABI
	elf.Write(make([]byte, 8))
	binary.Write(&elf, binary.LittleEndian, uint16(2))  // ET_EXEC
	binary.Write(&elf, binary.LittleEndian, uint16(62)) // EM_X86_64
	binary.Write(&elf, binary.LittleEndian, uint32(1))  // e_version
	binary.Write(&elf, binary.LittleEndian, loadAddr)    // e_entry
	binary.Write(&elf, binary.LittleEndian, uint64(64))  // e_phoff
	binary.Write(&elf, binary.LittleEndian, uint64(0))   // e_shoff
	binary.Write(&elf, binary.LittleEndian, uint32(0))   // e_flags
	binary.Write(&elf, binary.LittleEndian, uint16(64))  // e_ehsize
	binary.Write(&elf, binary.LittleEndian, uint16(56))  // e_phentsize
	binary.Write(&elf, binary.LittleEndian, uint16(1))   // e_phnum
	binary.Write(&elf, binary.LittleEndian, uint16(0))   // e_shentsize
	binary.Write(&elf, binary.LittleEndian, uint16(0))   // e_shnum
	binary.Write(&elf, binary.LittleEndian, uint16(0))   // e_shstrndx
	// Program header
	dataOff := uint64(64 + 56)
	binary.Write(&elf, binary.LittleEndian, uint32(1))     // PT_LOAD
	binary.Write(&elf, binary.LittleEndian, uint32(5))     // PF_R|PF_X
	binary.Write(&elf, binary.LittleEndian, dataOff)       // p_offset
	binary.Write(&elf, binary.LittleEndian, loadAddr)      // p_vaddr
	binary.Write(&elf, binary.LittleEndian, loadAddr)      // p_paddr
	binary.Write(&elf, binary.LittleEndian, uint64(len(code))) // p_filesz
	binary.Write(&elf, binary.LittleEndian, uint64(len(code))) // p_memsz
	binary.Write(&elf, binary.LittleEndian, uint64(0x1000))    // p_align
	elf.Write(code)
	return elf.Bytes()
}

func TestWriteE820MapSmallMemory(t *testing.T) {
	mem := make([]byte, 256*1024)
	// Memory smaller than 1MB should still work
	writeE820Map(mem, 0, 0x80000)
	if mem[0x1E8] != 4 {
		t.Fatalf("e820 count = %d, want 4", mem[0x1E8])
	}
}

func TestNormalizeKernelImage_ELFFile(t *testing.T) {
	code := bytes.Repeat([]byte{0xCC}, 64)
	elfData := makeMinimalELF(0x100000, code)
	path := t.TempDir() + "/vmlinux"
	if err := writeFile(path, elfData); err != nil {
		t.Fatal(err)
	}
	out, err := NormalizeKernelImage(path)
	if err != nil {
		t.Fatalf("NormalizeKernelImage: %v", err)
	}
	if !bytes.Equal(out, elfData) {
		t.Fatal("ELF should pass through unchanged")
	}
}

func TestNormalizeKernelImage_MissingFile(t *testing.T) {
	_, err := NormalizeKernelImage("/nonexistent/path")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadArm64Kernel_File(t *testing.T) {
	mem := make([]byte, 16*1024*1024)
	image := makeARM64Image(4096, 0x80000, 4096)
	path := t.TempDir() + "/Image"
	if err := writeFile(path, image); err != nil {
		t.Fatal(err)
	}
	info, err := LoadArm64Kernel(mem, path, 0x40000000)
	if err != nil {
		t.Fatalf("LoadArm64Kernel: %v", err)
	}
	if info.EntryPoint != 0x40080000 {
		t.Fatalf("entry = %#x", info.EntryPoint)
	}
}

func TestLoadArm64Kernel_MissingFile(t *testing.T) {
	mem := make([]byte, 16*1024*1024)
	_, err := LoadArm64Kernel(mem, "/nonexistent", 0x40000000)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadArm64KernelBytes_ImageSizeZero(t *testing.T) {
	mem := make([]byte, 16*1024*1024)
	// Create image with imageSize=0 and textOffset=0
	image := makeARM64Image(4096, 0, 0)
	info, err := loadArm64KernelBytes(mem, image, 0x40000000)
	if err != nil {
		t.Fatalf("loadArm64KernelBytes: %v", err)
	}
	// Should use default text offset 0x80000
	if info.EntryPoint != 0x40080000 {
		t.Fatalf("entry = %#x, want %#x", info.EntryPoint, 0x40080000)
	}
}

func TestLoadArm64KernelBytes_TooLargeForRAM(t *testing.T) {
	mem := make([]byte, 1024) // very small
	image := makeARM64Image(4096, 0x80000, 4096)
	_, err := loadArm64KernelBytes(mem, image, 0x40000000)
	if err == nil {
		t.Fatal("expected error for image exceeding RAM")
	}
}

func TestMatchCompressionXZ(t *testing.T) {
	// XZ magic: 0xFD 0x37 0x7A 0x58 0x5A 0x00
	data := []byte{0xFD, 0x37, 0x7A, 0x58, 0x5A, 0x00, 0x00, 0x00}
	_, found := matchCompression(data)
	// XZ needs valid stream, so NewReader may fail, but matchCompression
	// should handle this gracefully
	_ = found
}

func TestMatchCompressionLZMA(t *testing.T) {
	// LZMA magic: 0x5D 0x00
	data := []byte{0x5D, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	_, found := matchCompression(data)
	_ = found // may or may not match depending on data validity
}

func TestLoadELFSegmentTooLarge(t *testing.T) {
	code := bytes.Repeat([]byte{0xCC}, 128)
	elfData := makeMinimalELF(0x100000, code)
	
	// Use tiny memory so segment overflows
	mem := make([]byte, 0x100)
	_, err := loadELF(mem, elfData)
	if err == nil {
		t.Fatal("expected error for segment exceeding RAM")
	}
}

func TestDecompressKernelPayloadZstdPrecise(t *testing.T) {
	original := []byte("kernel from precise zstd payload")
	var buf bytes.Buffer
	w, _ := zstd.NewWriter(&buf)
	w.Write(original)
	w.Close()

	// With precise payload
	out, err := decompressKernelPayload([]byte{0x00, 0x00, 0x00, 0x00}, buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, original) {
		t.Fatalf("decompressed = %q, want %q", out, original)
	}
}

func TestExtractVmlinuxFromBzImageSetupOverflow(t *testing.T) {
	buf := make([]byte, 0x300)
	binary.LittleEndian.PutUint16(buf[bootFlag:], 0xAA55)
	binary.LittleEndian.PutUint32(buf[headerMagic:], 0x53726448)
	binary.LittleEndian.PutUint16(buf[protocolVer:], 0x020F)
	buf[setupSects] = 200 // overflow
	_, err := extractVmlinuxFromBzImage(buf)
	if err == nil {
		t.Fatal("expected error for setup overflow")
	}
}

func TestExtractVmlinuxNonELFPayload(t *testing.T) {
	// A bzImage whose payload decompresses to non-ELF data
	payload := []byte("not an ELF binary at all!!!!!")
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	gw.Write(payload)
	gw.Close()

	setupSecs := byte(4)
	hdr := makeBzImageHeader(setupSecs, gzBuf.Len())
	setupBytes := (int(setupSecs) + 1) * 512
	copy(hdr[setupBytes:], gzBuf.Bytes())

	_, err := extractVmlinuxFromBzImage(hdr)
	if err == nil {
		t.Fatal("expected error for non-ELF payload")
	}
}

func TestLoadBzImageWithPayloadOffset(t *testing.T) {
	// Create a bzImage with payload_offset and payload_length set
	payload := bytes.Repeat([]byte{0x90}, 128)
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	gw.Write(payload)
	gw.Close()

	setupSecs := byte(4)
	hdr := makeBzImageHeader(setupSecs, 512+gzBuf.Len())
	setupBytes := (int(setupSecs) + 1) * 512

	// Place the compressed data 512 bytes into the protected-mode area
	// Set payload_offset and payload_length
	pOff := 512
	copy(hdr[setupBytes+pOff:], gzBuf.Bytes())
	binary.LittleEndian.PutUint32(hdr[payloadOffset:], uint32(pOff))
	binary.LittleEndian.PutUint32(hdr[payloadLength:], uint32(gzBuf.Len()))

	mem := make([]byte, 4*1024*1024)
	info, err := loadBzImage(mem, hdr, 0x7000)
	if err != nil {
		t.Fatalf("loadBzImage: %v", err)
	}
	if info.EntryPoint != 0x100000 {
		t.Fatalf("entry = %#x", info.EntryPoint)
	}
}
