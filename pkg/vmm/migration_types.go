package vmm

// Migration data types used for serializing dirty page patches during
// live migration. These types are platform-neutral.

const (
	migrationMemFile    = "mem.bin"
	migrationPatchMeta  = "patches.json"
	migrationPatchData  = "patches.bin"
	migrationKernelPath = "artifacts/kernel"
	migrationInitrdPath = "artifacts/initrd"
	migrationDiskPath   = "artifacts/disk.ext4"
)

// DirtyPatchEntry describes a single dirty memory range within a file.
type DirtyPatchEntry struct {
	Offset     uint64 `json:"offset"`
	Length     uint64 `json:"length"`
	DataOffset uint64 `json:"data_offset"`
}

// DirtyFilePatch describes dirty pages for a single file.
type DirtyFilePatch struct {
	Path     string            `json:"path"`
	PageSize uint64            `json:"page_size"`
	Entries  []DirtyPatchEntry `json:"entries,omitempty"`
}

// MigrationPatchSet holds all dirty page patches for a migration.
type MigrationPatchSet struct {
	Version int              `json:"version"`
	Patches []DirtyFilePatch `json:"patches,omitempty"`
}
