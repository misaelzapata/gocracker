package fdt

import (
	"os"
	"testing"
)

func TestDumpARM64DTB(t *testing.T) {
	cfg := ARM64Config{
		MemBase:  0x80000000,
		MemBytes: 256 * 1024 * 1024,
		CPUs:     1,
		Cmdline:  "console=ttyAMA0 reboot=k panic=1",
		VirtioDevices: []VirtioDevice{
			{BaseAddr: 0x40003000, Size: 0x1000, IRQ: 2},
		},
	}
	dtb, err := GenerateARM64(cfg)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile("/tmp/test-arm64.dtb", dtb, 0644)
	t.Logf("DTB size: %d bytes", len(dtb))
	t.Logf("DTB magic: %x", dtb[:4])
}
