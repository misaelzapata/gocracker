package ext4

import (
	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
)

// openImage opens an existing ext4 image read-only. Helper for tests
// and inspection.
func openImage(path string) (*disk.Disk, error) {
	return diskfs.Open(path, diskfs.WithOpenMode(diskfs.ReadOnly))
}

// openExt4 reads the ext4 filesystem inside an opened disk. Helper.
func openExt4(d *disk.Disk) (*ext4.FileSystem, error) {
	return ext4.Read(d.Backend, d.Size, 0, d.LogicalBlocksize)
}
