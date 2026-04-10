// Package ext4write provides minimal ext4 file manipulation in pure Go.
// It can write or overwrite a file inside an ext4 disk image without
// requiring external tools like debugfs.
package ext4write

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path"
	"strings"
)

const (
	superblockOffset  = 1024
	inodeSize128      = 128
	rootInode         = 2
	ext4MagicNumber   = 0xEF53
	dirEntryHeaderLen = 8
	blockSizeDefault  = 4096
)

// WriteFile writes data to the given path inside an ext4 disk image.
// It creates parent directories as needed. The file is created if it does
// not exist, or overwritten if it does (provided there is enough space in
// the existing allocation).
func WriteFile(diskPath, filePath string, data []byte) error {
	f, err := os.OpenFile(diskPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open disk image: %w", err)
	}
	defer f.Close()

	img := &image{f: f}
	if err := img.readSuperblock(); err != nil {
		return err
	}

	// Walk to parent directory, creating intermediate dirs
	dir := path.Dir(filePath)
	base := path.Base(filePath)
	parentInode := uint32(rootInode)
	if dir != "/" && dir != "." {
		parts := strings.Split(strings.Trim(dir, "/"), "/")
		for _, part := range parts {
			child, err := img.lookupDirEntry(parentInode, part)
			if err != nil {
				child, err = img.createDir(parentInode, part)
				if err != nil {
					return fmt.Errorf("mkdir %s: %w", part, err)
				}
			}
			parentInode = child
		}
	}

	// Remove existing file if present
	existingInode, err := img.lookupDirEntry(parentInode, base)
	if err == nil {
		_ = img.removeDirectoryEntry(parentInode, base)
		img.freeInode(existingInode)
	}

	// Allocate new inode and blocks
	fileInode, err := img.allocInode()
	if err != nil {
		return fmt.Errorf("alloc inode: %w", err)
	}

	blocks, err := img.allocBlocks(int(math.Ceil(float64(len(data)) / float64(img.blockSize))))
	if err != nil {
		return fmt.Errorf("alloc blocks: %w", err)
	}

	// Write file data to blocks
	for i, blk := range blocks {
		start := i * int(img.blockSize)
		end := start + int(img.blockSize)
		if end > len(data) {
			end = len(data)
		}
		chunk := make([]byte, img.blockSize)
		copy(chunk, data[start:end])
		if err := img.writeBlock(blk, chunk); err != nil {
			return fmt.Errorf("write block: %w", err)
		}
	}

	// Write inode (simplified: uses extent tree with single extent)
	if err := img.writeFileInode(fileInode, uint64(len(data)), blocks); err != nil {
		return fmt.Errorf("write inode: %w", err)
	}

	// Add directory entry
	if err := img.addDirectoryEntry(parentInode, base, fileInode, 1); err != nil {
		return fmt.Errorf("add dir entry: %w", err)
	}

	return nil
}

// RemoveFile removes a file from an ext4 disk image.
func RemoveFile(diskPath, filePath string) error {
	f, err := os.OpenFile(diskPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open disk image: %w", err)
	}
	defer f.Close()

	img := &image{f: f}
	if err := img.readSuperblock(); err != nil {
		return err
	}

	dir := path.Dir(filePath)
	base := path.Base(filePath)
	parentInode := uint32(rootInode)
	if dir != "/" && dir != "." {
		parts := strings.Split(strings.Trim(dir, "/"), "/")
		for _, part := range parts {
			child, err := img.lookupDirEntry(parentInode, part)
			if err != nil {
				return fmt.Errorf("path not found: %s", dir)
			}
			parentInode = child
		}
	}

	inode, err := img.lookupDirEntry(parentInode, base)
	if err != nil {
		return nil // file doesn't exist, nothing to remove
	}
	_ = img.removeDirectoryEntry(parentInode, base)
	img.freeInode(inode)
	return nil
}

// image wraps an ext4 disk image file.
type image struct {
	f              *os.File
	blockSize      uint32
	inodesPerGroup uint32
	blocksPerGroup uint32
	inodeSize      uint16
	groupDescSize  uint16
	totalInodes    uint32
	totalBlocks    uint32
	firstDataBlock uint32
}

func (img *image) readSuperblock() error {
	buf := make([]byte, 1024)
	if _, err := img.f.ReadAt(buf, superblockOffset); err != nil {
		return fmt.Errorf("read superblock: %w", err)
	}
	magic := binary.LittleEndian.Uint16(buf[0x38:])
	if magic != ext4MagicNumber {
		return fmt.Errorf("not an ext4 filesystem (magic=%#x)", magic)
	}
	logBlockSize := binary.LittleEndian.Uint32(buf[0x18:])
	img.blockSize = 1024 << logBlockSize
	img.inodesPerGroup = binary.LittleEndian.Uint32(buf[0x28:])
	img.blocksPerGroup = binary.LittleEndian.Uint32(buf[0x20:])
	img.inodeSize = binary.LittleEndian.Uint16(buf[0x58:])
	img.totalInodes = binary.LittleEndian.Uint32(buf[0x00:])
	img.totalBlocks = binary.LittleEndian.Uint32(buf[0x04:])
	img.firstDataBlock = binary.LittleEndian.Uint32(buf[0x14:])

	// 64-bit feature check for group descriptor size
	featureIncompat := binary.LittleEndian.Uint32(buf[0x60:])
	if featureIncompat&0x80 != 0 { // INCOMPAT_64BIT
		img.groupDescSize = binary.LittleEndian.Uint16(buf[0xFE:])
	}
	if img.groupDescSize < 32 {
		img.groupDescSize = 32
	}
	if img.inodeSize == 0 {
		img.inodeSize = inodeSize128
	}
	return nil
}

func (img *image) readBlock(blk uint32) ([]byte, error) {
	buf := make([]byte, img.blockSize)
	_, err := img.f.ReadAt(buf, int64(blk)*int64(img.blockSize))
	return buf, err
}

func (img *image) writeBlock(blk uint32, data []byte) error {
	_, err := img.f.WriteAt(data, int64(blk)*int64(img.blockSize))
	return err
}

func (img *image) groupDescOffset(group uint32) int64 {
	gdBlock := img.firstDataBlock + 1 // group descriptors start at block after superblock
	return int64(gdBlock)*int64(img.blockSize) + int64(group)*int64(img.groupDescSize)
}

func (img *image) inodeOffset(inode uint32) int64 {
	inode-- // inodes are 1-indexed
	group := inode / img.inodesPerGroup
	index := inode % img.inodesPerGroup

	// Read inode table location from group descriptor
	gdOff := img.groupDescOffset(group)
	gd := make([]byte, img.groupDescSize)
	img.f.ReadAt(gd, gdOff)
	inodeTableLow := binary.LittleEndian.Uint32(gd[8:])
	inodeTableHigh := uint64(0)
	if img.groupDescSize > 32 {
		inodeTableHigh = uint64(binary.LittleEndian.Uint32(gd[40:]))
	}
	inodeTable := uint64(inodeTableLow) | (inodeTableHigh << 32)
	return int64(inodeTable)*int64(img.blockSize) + int64(index)*int64(img.inodeSize)
}

func (img *image) readInode(inode uint32) ([]byte, error) {
	buf := make([]byte, img.inodeSize)
	_, err := img.f.ReadAt(buf, img.inodeOffset(inode))
	return buf, err
}

func (img *image) writeInodeRaw(inode uint32, data []byte) error {
	_, err := img.f.WriteAt(data, img.inodeOffset(inode))
	return err
}

func (img *image) lookupDirEntry(dirInode uint32, name string) (uint32, error) {
	inodeBuf, err := img.readInode(dirInode)
	if err != nil {
		return 0, err
	}
	// Get block pointers from inode (extent tree or direct blocks)
	blocks := img.getInodeBlocks(inodeBuf)
	for _, blk := range blocks {
		dirData, err := img.readBlock(blk)
		if err != nil {
			continue
		}
		off := 0
		for off < int(img.blockSize) {
			if off+dirEntryHeaderLen > int(img.blockSize) {
				break
			}
			entInode := binary.LittleEndian.Uint32(dirData[off:])
			recLen := binary.LittleEndian.Uint16(dirData[off+4:])
			nameLen := dirData[off+6]
			if recLen == 0 {
				break
			}
			if entInode != 0 && int(nameLen) == len(name) && off+8+int(nameLen) <= int(img.blockSize) {
				entName := string(dirData[off+8 : off+8+int(nameLen)])
				if entName == name {
					return entInode, nil
				}
			}
			off += int(recLen)
		}
	}
	return 0, fmt.Errorf("not found: %s", name)
}

func (img *image) getInodeBlocks(inodeBuf []byte) []uint32 {
	// Check if uses extent tree (flag 0x80000 in i_flags at offset 0x20)
	flags := binary.LittleEndian.Uint32(inodeBuf[0x20:])
	if flags&0x80000 != 0 { // EXT4_EXTENTS_FL
		return img.parseExtentTree(inodeBuf[0x28:]) // i_block starts at offset 0x28
	}
	// Direct block pointers
	var blocks []uint32
	for i := 0; i < 12; i++ {
		blk := binary.LittleEndian.Uint32(inodeBuf[0x28+i*4:])
		if blk != 0 {
			blocks = append(blocks, blk)
		}
	}
	return blocks
}

func (img *image) parseExtentTree(data []byte) []uint32 {
	if len(data) < 12 {
		return nil
	}
	// Extent header
	magic := binary.LittleEndian.Uint16(data[0:])
	entries := binary.LittleEndian.Uint16(data[2:])
	depth := binary.LittleEndian.Uint16(data[6:])
	if magic != 0xF30A {
		return nil
	}
	var blocks []uint32
	if depth == 0 { // leaf
		for i := uint16(0); i < entries; i++ {
			off := 12 + i*12
			numBlocks := binary.LittleEndian.Uint16(data[off+4:])
			startLo := binary.LittleEndian.Uint32(data[off+8:])
			for j := uint16(0); j < numBlocks; j++ {
				blocks = append(blocks, startLo+uint32(j))
			}
		}
	}
	return blocks
}

func (img *image) allocInode() (uint32, error) {
	numGroups := (img.totalInodes + img.inodesPerGroup - 1) / img.inodesPerGroup
	for g := uint32(0); g < numGroups; g++ {
		gdOff := img.groupDescOffset(g)
		gd := make([]byte, img.groupDescSize)
		img.f.ReadAt(gd, gdOff)
		freeInodes := binary.LittleEndian.Uint16(gd[14:])
		if freeInodes == 0 {
			continue
		}
		// Read inode bitmap
		bitmapBlockLow := binary.LittleEndian.Uint32(gd[4:])
		bitmap, _ := img.readBlock(bitmapBlockLow)
		for i := uint32(0); i < img.inodesPerGroup; i++ {
			byteIdx := i / 8
			bitIdx := i % 8
			if bitmap[byteIdx]&(1<<bitIdx) == 0 {
				// Found free inode
				bitmap[byteIdx] |= 1 << bitIdx
				img.writeBlock(bitmapBlockLow, bitmap)
				// Update free count
				binary.LittleEndian.PutUint16(gd[14:], freeInodes-1)
				img.f.WriteAt(gd, gdOff)
				// Update superblock free inodes
				img.updateSuperblockFreeInodes(-1)
				return g*img.inodesPerGroup + i + 1, nil
			}
		}
	}
	return 0, fmt.Errorf("no free inodes")
}

func (img *image) allocBlocks(count int) ([]uint32, error) {
	if count == 0 {
		return nil, nil
	}
	var allocated []uint32
	numGroups := (img.totalBlocks + img.blocksPerGroup - 1) / img.blocksPerGroup
	for g := uint32(0); g < numGroups && len(allocated) < count; g++ {
		gdOff := img.groupDescOffset(g)
		gd := make([]byte, img.groupDescSize)
		img.f.ReadAt(gd, gdOff)
		freeBlocks := binary.LittleEndian.Uint16(gd[12:])
		if freeBlocks == 0 {
			continue
		}
		bitmapBlockLow := binary.LittleEndian.Uint32(gd[0:])
		bitmap, _ := img.readBlock(bitmapBlockLow)
		usedFromGroup := 0
		for i := uint32(0); i < img.blocksPerGroup && len(allocated) < count; i++ {
			byteIdx := i / 8
			bitIdx := i % 8
			if bitmap[byteIdx]&(1<<bitIdx) == 0 {
				bitmap[byteIdx] |= 1 << bitIdx
				allocated = append(allocated, g*img.blocksPerGroup+i+img.firstDataBlock)
				usedFromGroup++
			}
		}
		if usedFromGroup > 0 {
			img.writeBlock(bitmapBlockLow, bitmap)
			binary.LittleEndian.PutUint16(gd[12:], freeBlocks-uint16(usedFromGroup))
			img.f.WriteAt(gd, gdOff)
		}
	}
	if len(allocated) < count {
		return nil, fmt.Errorf("not enough free blocks (need %d, got %d)", count, len(allocated))
	}
	img.updateSuperblockFreeBlocks(-int32(count))
	return allocated, nil
}

func (img *image) writeFileInode(inode uint32, size uint64, blocks []uint32) error {
	buf := make([]byte, img.inodeSize)
	// i_mode: regular file, 0644
	binary.LittleEndian.PutUint16(buf[0x00:], 0x81A4)
	// i_size_lo
	binary.LittleEndian.PutUint32(buf[0x04:], uint32(size))
	// i_links_count
	binary.LittleEndian.PutUint16(buf[0x1A:], 1)
	// i_blocks_lo (in 512-byte units)
	sectorCount := uint32(len(blocks)) * (img.blockSize / 512)
	binary.LittleEndian.PutUint32(buf[0x1C:], sectorCount)
	// i_flags: EXT4_EXTENTS_FL
	binary.LittleEndian.PutUint32(buf[0x20:], 0x80000)
	// i_size_high
	binary.LittleEndian.PutUint32(buf[0x6C:], uint32(size>>32))

	// Write extent tree at i_block (offset 0x28)
	ext := buf[0x28:]
	// Extent header
	binary.LittleEndian.PutUint16(ext[0:], 0xF30A) // magic
	if len(blocks) > 0 {
		binary.LittleEndian.PutUint16(ext[2:], 1) // entries
	}
	binary.LittleEndian.PutUint16(ext[4:], 4)  // max entries
	binary.LittleEndian.PutUint16(ext[6:], 0)  // depth (leaf)
	binary.LittleEndian.PutUint32(ext[8:], 0)  // generation

	if len(blocks) > 0 {
		// Single extent covering all blocks (assumes contiguous)
		e := ext[12:]
		binary.LittleEndian.PutUint32(e[0:], 0)                  // ee_block (logical)
		binary.LittleEndian.PutUint16(e[4:], uint16(len(blocks))) // ee_len
		binary.LittleEndian.PutUint16(e[6:], 0)                  // ee_start_hi
		binary.LittleEndian.PutUint32(e[8:], blocks[0])          // ee_start_lo
	}

	return img.writeInodeRaw(inode, buf)
}

func (img *image) addDirectoryEntry(dirInode uint32, name string, fileInode uint32, fileType uint8) error {
	inodeBuf, err := img.readInode(dirInode)
	if err != nil {
		return err
	}
	blocks := img.getInodeBlocks(inodeBuf)
	if len(blocks) == 0 {
		return fmt.Errorf("directory has no blocks")
	}

	recLen := uint16(((8 + len(name)) + 3) & ^3) // align to 4 bytes

	// Try to find space in existing blocks
	for _, blk := range blocks {
		dirData, err := img.readBlock(blk)
		if err != nil {
			continue
		}
		off := 0
		for off < int(img.blockSize) {
			entInode := binary.LittleEndian.Uint32(dirData[off:])
			entRecLen := binary.LittleEndian.Uint16(dirData[off+4:])
			entNameLen := dirData[off+6]
			if entRecLen == 0 {
				break
			}
			actualLen := uint16(((8 + int(entNameLen)) + 3) & ^3)
			if entInode == 0 && entRecLen >= recLen {
				// Reuse deleted entry
				binary.LittleEndian.PutUint32(dirData[off:], fileInode)
				binary.LittleEndian.PutUint16(dirData[off+4:], entRecLen)
				dirData[off+6] = uint8(len(name))
				dirData[off+7] = fileType
				copy(dirData[off+8:], name)
				return img.writeBlock(blk, dirData)
			}
			if entInode != 0 && entRecLen >= actualLen+recLen {
				// Split this entry
				newOff := off + int(actualLen)
				remaining := entRecLen - actualLen
				binary.LittleEndian.PutUint16(dirData[off+4:], actualLen)
				binary.LittleEndian.PutUint32(dirData[newOff:], fileInode)
				binary.LittleEndian.PutUint16(dirData[newOff+4:], remaining)
				dirData[newOff+6] = uint8(len(name))
				dirData[newOff+7] = fileType
				copy(dirData[newOff+8:], name)
				return img.writeBlock(blk, dirData)
			}
			off += int(entRecLen)
		}
	}
	return fmt.Errorf("no space in directory for entry %q", name)
}

func (img *image) removeDirectoryEntry(dirInode uint32, name string) error {
	inodeBuf, err := img.readInode(dirInode)
	if err != nil {
		return err
	}
	blocks := img.getInodeBlocks(inodeBuf)
	for _, blk := range blocks {
		dirData, err := img.readBlock(blk)
		if err != nil {
			continue
		}
		off := 0
		for off < int(img.blockSize) {
			entInode := binary.LittleEndian.Uint32(dirData[off:])
			entRecLen := binary.LittleEndian.Uint16(dirData[off+4:])
			entNameLen := dirData[off+6]
			if entRecLen == 0 {
				break
			}
			if entInode != 0 && int(entNameLen) == len(name) {
				entName := string(dirData[off+8 : off+8+int(entNameLen)])
				if entName == name {
					binary.LittleEndian.PutUint32(dirData[off:], 0) // zero the inode
					return img.writeBlock(blk, dirData)
				}
			}
			off += int(entRecLen)
		}
	}
	return nil
}

func (img *image) createDir(parentInode uint32, name string) (uint32, error) {
	newInode, err := img.allocInode()
	if err != nil {
		return 0, err
	}
	blocks, err := img.allocBlocks(1)
	if err != nil {
		return 0, err
	}
	// Initialize directory block with . and ..
	dirBlock := make([]byte, img.blockSize)
	// . entry
	binary.LittleEndian.PutUint32(dirBlock[0:], newInode)
	binary.LittleEndian.PutUint16(dirBlock[4:], 12)
	dirBlock[6] = 1
	dirBlock[7] = 2 // directory type
	dirBlock[8] = '.'
	// .. entry
	binary.LittleEndian.PutUint32(dirBlock[12:], parentInode)
	binary.LittleEndian.PutUint16(dirBlock[16:], uint16(img.blockSize)-12)
	dirBlock[18] = 2
	dirBlock[19] = 2
	dirBlock[20] = '.'
	dirBlock[21] = '.'
	img.writeBlock(blocks[0], dirBlock)

	// Write directory inode
	buf := make([]byte, img.inodeSize)
	binary.LittleEndian.PutUint16(buf[0x00:], 0x41ED) // S_IFDIR | 0755
	binary.LittleEndian.PutUint32(buf[0x04:], img.blockSize)
	binary.LittleEndian.PutUint16(buf[0x1A:], 2) // links: . and ..
	binary.LittleEndian.PutUint32(buf[0x1C:], img.blockSize/512)
	binary.LittleEndian.PutUint32(buf[0x20:], 0x80000) // extents flag
	ext := buf[0x28:]
	binary.LittleEndian.PutUint16(ext[0:], 0xF30A)
	binary.LittleEndian.PutUint16(ext[2:], 1)
	binary.LittleEndian.PutUint16(ext[4:], 4)
	binary.LittleEndian.PutUint32(ext[12+0:], 0)
	binary.LittleEndian.PutUint16(ext[12+4:], 1)
	binary.LittleEndian.PutUint32(ext[12+8:], blocks[0])
	img.writeInodeRaw(newInode, buf)

	// Add entry in parent
	if err := img.addDirectoryEntry(parentInode, name, newInode, 2); err != nil {
		return 0, err
	}
	return newInode, nil
}

func (img *image) freeInode(inode uint32) {
	// Mark inode as free in bitmap
	inode--
	group := inode / img.inodesPerGroup
	index := inode % img.inodesPerGroup
	gdOff := img.groupDescOffset(group)
	gd := make([]byte, img.groupDescSize)
	img.f.ReadAt(gd, gdOff)
	bitmapBlock := binary.LittleEndian.Uint32(gd[4:])
	bitmap, _ := img.readBlock(bitmapBlock)
	bitmap[index/8] &^= 1 << (index % 8)
	img.writeBlock(bitmapBlock, bitmap)
	freeInodes := binary.LittleEndian.Uint16(gd[14:])
	binary.LittleEndian.PutUint16(gd[14:], freeInodes+1)
	img.f.WriteAt(gd, gdOff)
	img.updateSuperblockFreeInodes(1)
}

func (img *image) updateSuperblockFreeInodes(delta int32) {
	buf := make([]byte, 4)
	img.f.ReadAt(buf, superblockOffset+0x10)
	count := int32(binary.LittleEndian.Uint32(buf))
	count += delta
	binary.LittleEndian.PutUint32(buf, uint32(count))
	img.f.WriteAt(buf, superblockOffset+0x10)
}

func (img *image) updateSuperblockFreeBlocks(delta int32) {
	buf := make([]byte, 4)
	img.f.ReadAt(buf, superblockOffset+0x0C)
	count := int32(binary.LittleEndian.Uint32(buf))
	count += delta
	binary.LittleEndian.PutUint32(buf, uint32(count))
	img.f.WriteAt(buf, superblockOffset+0x0C)
}
