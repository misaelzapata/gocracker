package fdt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
)

// ---------- helpers ----------

// readBE32 reads a big-endian uint32 at offset from b.
func readBE32(b []byte, off int) uint32 {
	return binary.BigEndian.Uint32(b[off:])
}

// scanTokens walks the struct block and returns all token values found.
func scanTokens(structBlock []byte) []uint32 {
	var tokens []uint32
	off := 0
	for off+4 <= len(structBlock) {
		tok := binary.BigEndian.Uint32(structBlock[off:])
		tokens = append(tokens, tok)
		switch tok {
		case tokenBeginNode:
			off += 4
			// skip name (null-terminated, padded to 4 bytes)
			for off < len(structBlock) && structBlock[off] != 0 {
				off++
			}
			// skip null + padding
			off++
			for off%4 != 0 {
				off++
			}
		case tokenEndNode:
			off += 4
		case tokenProp:
			off += 4
			if off+8 > len(structBlock) {
				return tokens
			}
			valLen := int(binary.BigEndian.Uint32(structBlock[off:]))
			off += 8 // skip len + nameoff
			off += valLen
			for off%4 != 0 {
				off++
			}
		case tokenEnd:
			off += 4
		case tokenNop:
			off += 4
		default:
			// Unknown token, stop
			return tokens
		}
	}
	return tokens
}

// countToken counts how many times a given token appears in the token stream.
func countToken(tokens []uint32, tok uint32) int {
	n := 0
	for _, t := range tokens {
		if t == tok {
			n++
		}
	}
	return n
}

// ---------- tests ----------

func TestDTBMagic(t *testing.T) {
	dtb, err := Generate(128*1024*1024, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(dtb) < 40 {
		t.Fatalf("DTB too small: %d bytes", len(dtb))
	}

	magic := readBE32(dtb, 0)
	if magic != 0xD00DFEED {
		t.Fatalf("DTB magic: got %#x, want 0xD00DFEED", magic)
	}
}

func TestDTBVersion(t *testing.T) {
	dtb, err := Generate(128*1024*1024, 1, nil)
	if err != nil {
		t.Fatal(err)
	}

	version := readBE32(dtb, 20)
	if version != 17 {
		t.Fatalf("DTB version: got %d, want 17", version)
	}

	lastCompat := readBE32(dtb, 24)
	if lastCompat != 16 {
		t.Fatalf("DTB last compat version: got %d, want 16", lastCompat)
	}
}

func TestDTBTotalSize(t *testing.T) {
	dtb, err := Generate(128*1024*1024, 1, nil)
	if err != nil {
		t.Fatal(err)
	}

	totalSize := readBE32(dtb, 4)
	if totalSize != uint32(len(dtb)) {
		t.Fatalf("DTB totalsize: header says %d, actual %d", totalSize, len(dtb))
	}
}

func TestDTBAligned(t *testing.T) {
	dtb, err := Generate(128*1024*1024, 1, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(dtb)%8 != 0 {
		t.Fatalf("DTB size %d not 8-byte aligned", len(dtb))
	}
}

func TestDTBHeaderOffsets(t *testing.T) {
	dtb, err := Generate(128*1024*1024, 1, nil)
	if err != nil {
		t.Fatal(err)
	}

	offStruct := readBE32(dtb, 8)
	offStrings := readBE32(dtb, 12)
	sizeStrings := readBE32(dtb, 32)
	sizeStruct := readBE32(dtb, 36)
	totalSize := readBE32(dtb, 4)

	// 40-byte header + 16-byte mem reservation map = 56
	if offStruct != 56 {
		t.Fatalf("off_dt_struct: got %d, want 56", offStruct)
	}
	if offStrings != offStruct+sizeStruct {
		t.Fatalf("off_dt_strings: got %d, want %d", offStrings, offStruct+sizeStruct)
	}
	if offStrings+sizeStrings > totalSize {
		t.Fatalf("strings block overflows: strings_end=%d > total=%d", offStrings+sizeStrings, totalSize)
	}
}

func TestDTBStructureTokens(t *testing.T) {
	dtb, err := Generate(128*1024*1024, 1, nil)
	if err != nil {
		t.Fatal(err)
	}

	offStruct := int(readBE32(dtb, 8))
	sizeStruct := int(readBE32(dtb, 36))
	structBlock := dtb[offStruct : offStruct+sizeStruct]

	tokens := scanTokens(structBlock)
	if len(tokens) == 0 {
		t.Fatal("no tokens found in struct block")
	}

	// First token must be FDT_BEGIN_NODE (root node)
	if tokens[0] != tokenBeginNode {
		t.Fatalf("first token: got %#x, want %#x (BEGIN_NODE)", tokens[0], tokenBeginNode)
	}

	// Last token must be FDT_END
	if tokens[len(tokens)-1] != tokenEnd {
		t.Fatalf("last token: got %#x, want %#x (END)", tokens[len(tokens)-1], tokenEnd)
	}

	// Every BEGIN_NODE (including root) must have a matching END_NODE.
	begins := countToken(tokens, tokenBeginNode)
	ends := countToken(tokens, tokenEndNode)
	if begins != ends {
		t.Fatalf("BEGIN_NODE count (%d) != END_NODE count (%d)", begins, ends)
	}
}

func TestDTBNoVirtioDevices(t *testing.T) {
	dtb, err := Generate(64*1024*1024, 1, nil)
	if err != nil {
		t.Fatal(err)
	}

	offStruct := int(readBE32(dtb, 8))
	sizeStruct := int(readBE32(dtb, 36))
	structBlock := dtb[offStruct : offStruct+sizeStruct]
	tokens := scanTokens(structBlock)

	begins := countToken(tokens, tokenBeginNode)
	// Without virtio devices we expect: root, chosen, memory, cpus, cpu@0, uart@3f8
	// = 6 BEGIN_NODE tokens
	if begins != 6 {
		t.Fatalf("BEGIN_NODE count with 0 virtio devices: got %d, want 6", begins)
	}
}

func TestGenerateARM64IncludesCoreNodes(t *testing.T) {
	dtb, err := GenerateARM64(ARM64Config{
		MemBase:    DefaultARM64MemoryBase,
		MemBytes:   128 * 1024 * 1024,
		CPUs:       2,
		Cmdline:    "console=ttyAMA0 root=/dev/vda",
		InitrdAddr: 0x48000000,
		InitrdSize: 0x200000,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"chosen",
		"aliases",
		fmt.Sprintf("memory@%x", DefaultARM64MemoryBase),
		"cpus",
		"cpu@0",
		"cpu@1",
		"psci",
		"intc",
		fmt.Sprintf("uart@%x", DefaultARM64PL011Base),
	} {
		if !bytes.Contains(dtb, append([]byte(name), 0)) {
			t.Fatalf("arm64 DTB missing node %q", name)
		}
	}
	if !bytes.Contains(dtb, []byte("bootargs\x00")) {
		t.Fatal("arm64 DTB missing bootargs property")
	}
	if !bytes.Contains(dtb, []byte("linux,initrd-start\x00")) {
		t.Fatal("arm64 DTB missing linux,initrd-start property")
	}
	if !bytes.Contains(dtb, []byte("linux,initrd-end\x00")) {
		t.Fatal("arm64 DTB missing linux,initrd-end property")
	}
}

func TestGenerateARM64IncludesVirtioNodes(t *testing.T) {
	dtb, err := GenerateARM64(ARM64Config{
		MemBase:  DefaultARM64MemoryBase,
		MemBytes: 256 * 1024 * 1024,
		CPUs:     1,
		VirtioDevices: []VirtioDevice{
			{BaseAddr: 0x0A000000, Size: 0x200, IRQ: 16},
			{BaseAddr: 0x0A000200, Size: 0x200, IRQ: 17},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"virtio_mmio@a000000", "virtio_mmio@a000200"} {
		if !bytes.Contains(dtb, append([]byte(name), 0)) {
			t.Fatalf("arm64 DTB missing virtio node %q", name)
		}
	}
	if !bytes.Contains(dtb, []byte("arm,gic-400\x00")) {
		t.Fatal("arm64 DTB missing GIC compatibility string")
	}
	if !bytes.Contains(dtb, []byte("ns16550a\x00")) {
		t.Fatal("arm64 DTB missing ns16550a compatibility string")
	}
}

func TestDTBTwoVirtioDevices(t *testing.T) {
	devs := []VirtioDevice{
		{BaseAddr: 0x10000000, Size: 0x200, IRQ: 33},
		{BaseAddr: 0x10000200, Size: 0x200, IRQ: 34},
	}
	dtb, err := Generate(128*1024*1024, 1, devs)
	if err != nil {
		t.Fatal(err)
	}

	magic := readBE32(dtb, 0)
	if magic != 0xD00DFEED {
		t.Fatalf("DTB magic: got %#x, want 0xD00DFEED", magic)
	}

	offStruct := int(readBE32(dtb, 8))
	sizeStruct := int(readBE32(dtb, 36))
	structBlock := dtb[offStruct : offStruct+sizeStruct]
	tokens := scanTokens(structBlock)

	begins := countToken(tokens, tokenBeginNode)
	ends := countToken(tokens, tokenEndNode)
	// 6 base nodes + 2 virtio = 8
	if begins != 8 {
		t.Fatalf("BEGIN_NODE count with 2 virtio devices: got %d, want 8", begins)
	}
	if begins != ends {
		t.Fatalf("BEGIN_NODE (%d) != END_NODE (%d)", begins, ends)
	}

	// Verify total size is still valid
	totalSize := readBE32(dtb, 4)
	if totalSize != uint32(len(dtb)) {
		t.Fatalf("totalsize mismatch: header %d vs actual %d", totalSize, len(dtb))
	}
}

func TestDTBMultipleCPUs(t *testing.T) {
	dtb, err := Generate(128*1024*1024, 4, nil)
	if err != nil {
		t.Fatal(err)
	}

	offStruct := int(readBE32(dtb, 8))
	sizeStruct := int(readBE32(dtb, 36))
	structBlock := dtb[offStruct : offStruct+sizeStruct]
	tokens := scanTokens(structBlock)

	begins := countToken(tokens, tokenBeginNode)
	// root, chosen, memory, cpus, cpu@0..cpu@3, uart = 6 + 3 extra CPUs = 9
	if begins != 9 {
		t.Fatalf("BEGIN_NODE count with 4 CPUs: got %d, want 9", begins)
	}
}

func TestDTBStringTable(t *testing.T) {
	dtb, err := Generate(128*1024*1024, 1, nil)
	if err != nil {
		t.Fatal(err)
	}

	offStrings := int(readBE32(dtb, 12))
	sizeStrings := int(readBE32(dtb, 32))
	strBlock := dtb[offStrings : offStrings+sizeStrings]

	if len(strBlock) == 0 {
		t.Fatal("string table is empty")
	}

	// The string table should contain known property names
	str := string(strBlock)
	expected := []string{"#address-cells", "#size-cells", "compatible", "device_type", "reg"}
	for _, want := range expected {
		found := false
		for i := 0; i < len(str); {
			end := i
			for end < len(str) && str[end] != 0 {
				end++
			}
			if str[i:end] == want {
				found = true
				break
			}
			if end < len(str) {
				i = end + 1
			} else {
				break
			}
		}
		if !found {
			t.Fatalf("string table missing %q", want)
		}
	}
}

func TestDTBBootCPUID(t *testing.T) {
	dtb, err := Generate(128*1024*1024, 1, nil)
	if err != nil {
		t.Fatal(err)
	}

	bootCPU := readBE32(dtb, 28)
	if bootCPU != 0 {
		t.Fatalf("boot_cpuid_phys: got %d, want 0", bootCPU)
	}
}

func TestDTBMemReserveMapOffset(t *testing.T) {
	dtb, err := Generate(128*1024*1024, 1, nil)
	if err != nil {
		t.Fatal(err)
	}

	offMemRsvMap := readBE32(dtb, 16)
	if offMemRsvMap != 40 {
		t.Fatalf("off_mem_rsvmap: got %d, want 40 (right after header)", offMemRsvMap)
	}
	// Verify the reservation map is an empty terminator (16 zero bytes)
	for i := offMemRsvMap; i < offMemRsvMap+16; i++ {
		if dtb[i] != 0 {
			t.Fatalf("mem_rsvmap byte %d: got %d, want 0", i-offMemRsvMap, dtb[i])
		}
	}
}

func TestGenerateReturnsNoError(t *testing.T) {
	// Verify Generate does not error with various configs
	cases := []struct {
		name    string
		memSize uint64
		cpus    int
		devs    []VirtioDevice
	}{
		{"minimal", 16 * 1024 * 1024, 1, nil},
		{"multi-cpu", 256 * 1024 * 1024, 8, nil},
		{"with devices", 128 * 1024 * 1024, 2, []VirtioDevice{
			{BaseAddr: 0xA0000, Size: 0x200, IRQ: 33},
			{BaseAddr: 0xA0200, Size: 0x200, IRQ: 34},
			{BaseAddr: 0xA0400, Size: 0x200, IRQ: 35},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dtb, err := Generate(tc.memSize, tc.cpus, tc.devs)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if len(dtb) < 40 {
				t.Fatalf("DTB too small: %d bytes", len(dtb))
			}
			magic := readBE32(dtb, 0)
			if magic != 0xD00DFEED {
				t.Fatalf("magic: %#x", magic)
			}
		})
	}
}
