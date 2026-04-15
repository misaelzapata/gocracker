package virtio

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	gclog "github.com/gocracker/gocracker/internal/log"
	"golang.org/x/sys/unix"
)

// virtio-blk feature bits
const (
	BlkFeatureRO      = 1 << 5
	BlkFeatureFlush   = 1 << 9
	BlkFeatureDiscard = 1 << 13
)

// virtio-blk request types
const (
	BlkTIn      = 0 // read
	BlkTOut     = 1 // write
	BlkTFlush   = 4
	BlkTGetID   = 8
	BlkTDiscard = 11
)

// virtio-blk request status
const (
	BlkSOK    = 0
	BlkSIOErr = 1
	BlkSUnsup = 2
)

const (
	blkSectorSize          = 512
	blkIDBytes             = 20
	blkDiscardSegmentBytes = 16
)

// blkConfig is the virtio-blk device config space layout.
type blkConfig struct {
	Capacity uint64
	SizeMax  uint32
	SegMax   uint32
	_        [20]byte // geometry + topology (unused)
}

// BlockDevice is a virtio-blk device backed by a raw disk image file.
type BlockDevice struct {
	*Transport
	file     *os.File
	readOnly bool
	discard  bool
	sectors  uint64
	dirty    *DirtyTracker
	rl       *RateLimiter
}

type blkDiscardSegment struct {
	sector     uint64
	numSectors uint32
	flags      uint32
}

type blkRequest struct {
	reqType    uint32
	sector     uint64
	dataDescs  []Desc
	dataLen    uint32
	discards   []blkDiscardSegment
	statusDesc Desc
}

var probeDiscardSupport = detectDiscardSupport
var punchHole = func(fd int, off, len int64) error {
	return unix.Fallocate(fd, unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, len)
}

// discardSupportCache memoises the FALLOC_FL_PUNCH_HOLE probe per backing
// filesystem. The probe writes + syncs + punches a 512-byte hole in a temp
// file; that costs ~2–5 ms every time we open a block device, and the
// answer is a property of the filesystem, not the image. Keying on the
// parent directory is a cheap proxy (images in the same cache dir share a
// result); we also re-probe if the cached dir is gone so that stale entries
// do not poison later launches.
var discardSupportCache sync.Map // map[string]bool

// NewBlockDevice creates a virtio-blk device from a raw image file.
func NewBlockDevice(mem []byte, basePA uint64, irq uint8, imagePath string, readOnly bool, dirty *DirtyTracker, irqFn func(bool)) (*BlockDevice, error) {
	return NewBlockDeviceWithOptions(mem, basePA, irq, imagePath, readOnly, dirty, irqFn, BlockDeviceOptions{})
}

// BlockDeviceOptions lets callers opt out of discovery syscalls that are safe
// to skip when the answer is already known (e.g. restoring from a snapshot,
// where the guest already negotiated features and a probe against the host
// filesystem is wasted work).
type BlockDeviceOptions struct {
	// SkipDiscardProbe bypasses the FALLOC_FL_PUNCH_HOLE temp-file probe in
	// detectDiscardSupport. Callers using this MUST supply Discard below
	// with the value the device should advertise. Set by the snapshot
	// restore path, which knows from the captured Transport.drvFeatures
	// what the guest already accepted.
	SkipDiscardProbe bool
	Discard          bool
}

// NewBlockDeviceWithOptions is the low-level constructor used by restore and
// advanced callers. The plain NewBlockDevice keeps the old signature.
func NewBlockDeviceWithOptions(mem []byte, basePA uint64, irq uint8, imagePath string, readOnly bool, dirty *DirtyTracker, irqFn func(bool), opts BlockDeviceOptions) (*BlockDevice, error) {
	flags := os.O_RDWR
	if readOnly {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(imagePath, flags, 0)
	if err != nil {
		return nil, fmt.Errorf("open image %s: %w", imagePath, err)
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	sectors := uint64(fi.Size()) / 512

	discard := false
	switch {
	case readOnly:
		// read-only images never advertise discard
	case opts.SkipDiscardProbe:
		discard = opts.Discard
	default:
		discard = probeDiscardSupport(imagePath)
	}

	d := &BlockDevice{file: f, readOnly: readOnly, discard: discard, sectors: sectors, dirty: NewDirtyTracker(uint64(fi.Size()))}
	d.Transport = NewTransport(d, mem, basePA, irq, dirty, irqFn)
	return d, nil
}

func (d *BlockDevice) SetRateLimiter(rl *RateLimiter) {
	d.rl = rl
}

func (d *BlockDevice) DeviceID() uint32 { return DeviceIDBlock }
func (d *BlockDevice) DeviceFeatures() uint64 {
	f := uint64(BlkFeatureFlush)
	if d.readOnly {
		f |= BlkFeatureRO
	}
	if d.discard {
		f |= BlkFeatureDiscard
	}
	return f
}

func (d *BlockDevice) ConfigBytes() []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b[0:], d.sectors)
	binary.LittleEndian.PutUint32(b[8:], 0)    // size_max
	binary.LittleEndian.PutUint32(b[12:], 128) // seg_max
	return b
}

// HandleQueue processes the request queue (queue 0).
func (d *BlockDevice) HandleQueue(idx uint32, q *Queue) {
	if idx != 0 {
		return
	}
	if err := q.IterAvail(func(head uint16) {
		chain, err := q.WalkChain(head)
		if err != nil {
			gclog.VMM.Warn("virtio-blk invalid descriptor chain", "head", head, "error", err)
			_ = q.PushUsed(uint32(head), 0)
			return
		}
		req, err := d.parseRequest(q, chain)
		if err != nil {
			gclog.VMM.Warn("virtio-blk parse request failed", "head", head, "chain_len", len(chain), "error", err)
			_ = q.PushUsed(uint32(head), 0)
			return
		}
		status := byte(BlkSOK)
		writtenToMem := uint32(0)

		switch req.reqType {
		case BlkTIn: // read sectors -> guest
			if d.rl != nil {
				d.rl.Wait(uint64(req.dataLen), 1)
			}
			sector := req.sector
			for _, desc := range req.dataDescs {
				// sync.Pool allocation removal
				buf := GetBlkBuffer(desc.Len)
				n, err := d.file.ReadAt(buf, int64(sector)*blkSectorSize)
				if err != nil || n != len(buf) {
					PutBlkBuffer(buf)
					status = BlkSIOErr
					break
				}
				if err := q.GuestWrite(desc.Addr, buf); err != nil {
					PutBlkBuffer(buf)
					gclog.VMM.Warn("virtio-blk guest write failed", "head", head, "error", err)
					status = BlkSIOErr
					break
				}
				PutBlkBuffer(buf)
				writtenToMem += desc.Len
				sector += uint64(desc.Len) / blkSectorSize
			}

		case BlkTOut: // write guest -> sectors
			if d.readOnly {
				status = BlkSIOErr
				break
			}
			if d.rl != nil {
				d.rl.Wait(uint64(req.dataLen), 1)
			}
			sector := req.sector
			for _, desc := range req.dataDescs {
				buf := GetBlkBuffer(desc.Len)
				if err := q.GuestRead(desc.Addr, buf); err != nil {
					PutBlkBuffer(buf)
					gclog.VMM.Warn("virtio-blk guest read failed", "head", head, "error", err)
					status = BlkSIOErr
					break
				}
				n, err := d.file.WriteAt(buf, int64(sector)*blkSectorSize)
				if err != nil || n != len(buf) {
					PutBlkBuffer(buf)
					status = BlkSIOErr
					break
				}
				PutBlkBuffer(buf)
				d.dirty.Mark(sector*blkSectorSize, uint64(len(buf)))
				sector += uint64(desc.Len) / blkSectorSize
			}

		case BlkTFlush:
			if d.rl != nil {
				d.rl.Wait(0, 1)
			}
			if err := d.file.Sync(); err != nil {
				gclog.VMM.Warn("virtio-blk flush failed", "head", head, "error", err)
				status = BlkSIOErr
			}

		case BlkTGetID:
			if d.rl != nil {
				d.rl.Wait(uint64(req.dataLen), 1)
			}
			id := []byte("gocracker\x00")
			for _, desc := range req.dataDescs {
				n := uint32(len(id))
				if n > desc.Len {
					n = desc.Len
				}
				if err := q.GuestWrite(desc.Addr, id[:n]); err != nil {
					gclog.VMM.Warn("virtio-blk get-id guest write failed", "head", head, "error", err)
					status = BlkSIOErr
					break
				}
				writtenToMem += n
				break
			}

		case BlkTDiscard:
			if !d.discard {
				status = BlkSUnsup
				break
			}
			discardBytes := uint64(0)
			for _, seg := range req.discards {
				discardBytes += uint64(seg.numSectors) * blkSectorSize
			}
			if d.rl != nil {
				d.rl.Wait(discardBytes, 1)
			}
			for _, seg := range req.discards {
				if seg.numSectors == 0 {
					continue
				}
				if err := d.discardRange(seg.sector, seg.numSectors); err != nil {
					gclog.VMM.Warn("virtio-blk discard failed", "head", head, "sector", seg.sector, "num_sectors", seg.numSectors, "error", err)
					status = BlkSIOErr
					break
				}
			}

		default:
			status = BlkSUnsup
		}

		// Write status byte into last descriptor
		if err := q.GuestWrite(req.statusDesc.Addr, []byte{status}); err != nil {
			gclog.VMM.Warn("virtio-blk status write failed", "head", head, "error", err)
			_ = q.PushUsed(uint32(head), writtenToMem)
			return
		}
		_ = q.PushUsed(uint32(head), writtenToMem+1)
	}); err != nil {
		gclog.VMM.Warn("virtio-blk queue iteration failed", "error", err)
	}
}

func (d *BlockDevice) parseRequest(q *Queue, chain []Desc) (blkRequest, error) {
	if len(chain) < 2 {
		return blkRequest{}, fmt.Errorf("descriptor chain too short")
	}

	hdrDesc := chain[0]
	if hdrDesc.Flags&DescFlagWrite != 0 {
		return blkRequest{}, fmt.Errorf("header descriptor is write-only")
	}
	if hdrDesc.Len < 16 {
		return blkRequest{}, fmt.Errorf("header descriptor too small")
	}

	hdr := make([]byte, 16)
	if err := q.GuestRead(hdrDesc.Addr, hdr); err != nil {
		return blkRequest{}, err
	}
	req := blkRequest{
		reqType: binary.LittleEndian.Uint32(hdr[0:4]),
		sector:  binary.LittleEndian.Uint64(hdr[8:16]),
	}

	req.statusDesc = chain[len(chain)-1]
	if req.statusDesc.Flags&DescFlagWrite == 0 {
		return blkRequest{}, fmt.Errorf("status descriptor is not writable")
	}
	if req.statusDesc.Len < 1 {
		return blkRequest{}, fmt.Errorf("status descriptor too small")
	}

	req.dataDescs = chain[1 : len(chain)-1]
	switch req.reqType {
	case BlkTFlush:
		if len(req.dataDescs) != 0 {
			return blkRequest{}, fmt.Errorf("flush request has unexpected data descriptors")
		}
	case BlkTIn, BlkTOut, BlkTGetID, BlkTDiscard:
		if len(req.dataDescs) == 0 {
			return blkRequest{}, fmt.Errorf("request missing data descriptors")
		}
	}

	for _, desc := range req.dataDescs {
		req.dataLen += desc.Len
		switch req.reqType {
		case BlkTIn, BlkTGetID:
			if desc.Flags&DescFlagWrite == 0 {
				return blkRequest{}, fmt.Errorf("read/get-id data descriptor is not writable")
			}
		case BlkTOut, BlkTDiscard:
			if desc.Flags&DescFlagWrite != 0 {
				return blkRequest{}, fmt.Errorf("write data descriptor is writable")
			}
		}
	}

	switch req.reqType {
	case BlkTIn, BlkTOut:
		if req.dataLen%blkSectorSize != 0 {
			return blkRequest{}, fmt.Errorf("data length %d is not sector-aligned", req.dataLen)
		}
		topSector := req.sector + uint64(req.dataLen/blkSectorSize)
		if topSector > d.sectors {
			return blkRequest{}, fmt.Errorf("request out of bounds: sector=%d data_len=%d sectors=%d", req.sector, req.dataLen, d.sectors)
		}
	case BlkTGetID:
		if req.dataLen < blkIDBytes {
			return blkRequest{}, fmt.Errorf("get-id buffer too small: %d", req.dataLen)
		}
	case BlkTDiscard:
		if req.dataLen%blkDiscardSegmentBytes != 0 {
			return blkRequest{}, fmt.Errorf("discard segment data length %d is not %d-byte aligned", req.dataLen, blkDiscardSegmentBytes)
		}
		segments, err := d.readDiscardSegments(q, req.dataDescs, req.dataLen)
		if err != nil {
			return blkRequest{}, err
		}
		req.discards = segments
	}

	return req, nil
}

func (d *BlockDevice) readDiscardSegments(q *Queue, descs []Desc, totalLen uint32) ([]blkDiscardSegment, error) {
	if totalLen == 0 {
		return nil, fmt.Errorf("discard request missing segments")
	}
	buf := make([]byte, totalLen)
	cursor := uint32(0)
	for _, desc := range descs {
		if err := q.GuestRead(desc.Addr, buf[cursor:cursor+desc.Len]); err != nil {
			return nil, err
		}
		cursor += desc.Len
	}
	segments := make([]blkDiscardSegment, 0, totalLen/blkDiscardSegmentBytes)
	for off := uint32(0); off < totalLen; off += blkDiscardSegmentBytes {
		entry := buf[off : off+blkDiscardSegmentBytes]
		seg := blkDiscardSegment{
			sector:     binary.LittleEndian.Uint64(entry[0:8]),
			numSectors: binary.LittleEndian.Uint32(entry[8:12]),
			flags:      binary.LittleEndian.Uint32(entry[12:16]),
		}
		if seg.flags != 0 {
			return nil, fmt.Errorf("discard segment flags %#x are unsupported", seg.flags)
		}
		if seg.numSectors == 0 {
			segments = append(segments, seg)
			continue
		}
		topSector := seg.sector + uint64(seg.numSectors)
		if topSector < seg.sector || topSector > d.sectors {
			return nil, fmt.Errorf("discard segment out of bounds: sector=%d num_sectors=%d sectors=%d", seg.sector, seg.numSectors, d.sectors)
		}
		segments = append(segments, seg)
	}
	return segments, nil
}

func (d *BlockDevice) discardRange(sector uint64, numSectors uint32) error {
	if numSectors == 0 {
		return nil
	}
	off := int64(sector * blkSectorSize)
	length := int64(uint64(numSectors) * blkSectorSize)
	if err := punchHole(int(d.file.Fd()), off, length); err != nil {
		return err
	}
	if d.dirty != nil {
		d.dirty.Mark(uint64(off), uint64(length))
	}
	return nil
}

func detectDiscardSupport(imagePath string) bool {
	dir := filepath.Dir(imagePath)
	if v, ok := discardSupportCache.Load(dir); ok {
		return v.(bool)
	}
	result := detectDiscardSupportUncached(dir)
	discardSupportCache.Store(dir, result)
	return result
}

func detectDiscardSupportUncached(dir string) bool {
	f, err := os.CreateTemp(dir, ".gocracker-discard-*")
	if err != nil {
		return false
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()

	testData := make([]byte, blkSectorSize)
	for i := range testData {
		testData[i] = 0x5a
	}
	if _, err := f.WriteAt(testData, 0); err != nil {
		return false
	}
	if err := f.Sync(); err != nil {
		return false
	}
	if err := punchHole(int(f.Fd()), 0, blkSectorSize); err != nil {
		return false
	}
	return true
}

// Close releases the backing file.
func (d *BlockDevice) Close() error {
	return d.file.Close()
}

// PrepareSnapshot flushes host-side writes before the VM state is serialized.
func (d *BlockDevice) PrepareSnapshot() error {
	if d.readOnly {
		return nil
	}
	return d.file.Sync()
}

func (d *BlockDevice) ResetDirty() {
	if d.dirty != nil {
		d.dirty.Reset()
	}
}

func (d *BlockDevice) DirtyBitmapAndReset() []uint64 {
	if d.dirty == nil {
		return nil
	}
	return d.dirty.SnapshotAndReset()
}

func (d *BlockDevice) DirtyPageSize() uint64 {
	if d.dirty == nil {
		return 4096
	}
	return d.dirty.PageSize()
}

func (d *BlockDevice) SizeBytes() uint64 {
	return d.sectors * blkSectorSize
}

func (d *BlockDevice) ReadAt(p []byte, off int64) (int, error) {
	return d.file.ReadAt(p, off)
}

// String returns a description of the block device.
func (d *BlockDevice) String() string {
	return fmt.Sprintf("virtio-blk sectors=%d ro=%v", d.sectors, d.readOnly)
}

// Basic variable-size buffer pool for blk operations
var blkPools = map[uint32]*sync.Pool{
	512:  {New: func() any { return make([]byte, 512) }},
	1024: {New: func() any { return make([]byte, 1024) }},
	4096: {New: func() any { return make([]byte, 4096) }},
	8192: {New: func() any { return make([]byte, 8192) }},
}

func getPoolSize(l uint32) uint32 {
	if l <= 512 {
		return 512
	}
	if l <= 1024 {
		return 1024
	}
	if l <= 4096 {
		return 4096
	}
	if l <= 8192 {
		return 8192
	}
	return l
}

func GetBlkBuffer(l uint32) []byte {
	size := getPoolSize(l)
	pool, ok := blkPools[size]
	if !ok {
		return make([]byte, l)
	}
	buf := pool.Get().([]byte)
	return buf[:l]
}

func PutBlkBuffer(b []byte) {
	c := uint32(cap(b))
	if pool, ok := blkPools[c]; ok {
		pool.Put(b[:cap(b)])
	}
}
