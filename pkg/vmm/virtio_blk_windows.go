//go:build windows

package vmm

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync"
)

// VirtioBlk is a minimal virtio-blk-mmio device. It backs Linux's
// virtio_blk driver against a host file (typically an ext4 image),
// serving the requests the kernel issues during boot:
//
//   - VIRTIO_BLK_T_IN     (0) read N sectors
//   - VIRTIO_BLK_T_OUT    (1) write N sectors
//   - VIRTIO_BLK_T_FLUSH  (4) fsync
//   - VIRTIO_BLK_T_GET_ID (8) device ID string (used by udev)
//
// Ported from node-vmm's native/whp/virtio/blk.cc.
//
// Locking discipline:
//   - VirtioMmioBase has its own mutex for register state.
//   - mu (below) serialises file I/O and request handling. The queue is
//     drained on every QueueNotify in the writing goroutine; serializing
//     keeps disk writes ordered.
type VirtioBlk struct {
	base     *VirtioMmioBase
	mmioBase uint64 // MMIO GPA where this device's window starts
	mem      []byte // host-side view of guest RAM (aliased)
	file     *os.File
	sectors  uint64
	readOnly bool
	raiseIRQ func()

	mu sync.Mutex
}

// Virtio-blk feature bits (from include/uapi/linux/virtio_blk.h).
const (
	virtioBlkFRo    uint64 = 1 << 5 // VIRTIO_BLK_F_RO
	virtioBlkFFlush uint64 = 1 << 9 // VIRTIO_BLK_F_FLUSH
)

// Virtio-blk request types.
const (
	virtioBlkTIn    uint32 = 0
	virtioBlkTOut   uint32 = 1
	virtioBlkTFlush uint32 = 4
	virtioBlkTGetID uint32 = 8
)

// Virtio-blk status bytes.
const (
	virtioBlkSOk     uint8 = 0
	virtioBlkSIOErr  uint8 = 1
	virtioBlkSUnsupp uint8 = 2
)

// NewVirtioBlk opens path and returns a virtio-blk-mmio device at
// mmioBase. The device is wired with raiseIRQ — invoked after each
// queue notify so the caller can deliver an IRQ to the guest.
func NewVirtioBlk(mmioBase uint64, mem []byte, path string, readOnly bool, raiseIRQ func()) (*VirtioBlk, error) {
	flag := os.O_RDWR
	if readOnly {
		flag = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return nil, fmt.Errorf("open rootfs %s: %w", path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat rootfs %s: %w", path, err)
	}
	if fi.Size() < 0 {
		_ = f.Close()
		return nil, fmt.Errorf("negative rootfs size: %s", path)
	}
	devFeatures := VirtioFVersion1 | virtioBlkFFlush
	if readOnly {
		devFeatures |= virtioBlkFRo
	}
	d := &VirtioBlk{
		base: &VirtioMmioBase{
			DeviceID:    2, // virtio-blk
			DevFeatures: devFeatures,
		},
		mmioBase: mmioBase,
		mem:      mem,
		file:     f,
		sectors:  uint64(fi.Size()) / 512,
		readOnly: readOnly,
		raiseIRQ: raiseIRQ,
	}
	return d, nil
}

// MmioBase returns the guest physical address of this device's window.
func (d *VirtioBlk) MmioBase() uint64 { return d.mmioBase }

// MmioEnd returns the first GPA past this device's window (exclusive).
// The window is one page (4 KiB) — covers the common register block
// (offsets 0x000-0x0FF) and the device config (0x100-0x1FF).
func (d *VirtioBlk) MmioEnd() uint64 { return d.mmioBase + 0x1000 }

// HandlesAddr returns true if addr falls inside this device's window.
func (d *VirtioBlk) HandlesAddr(addr uint64) bool {
	return addr >= d.mmioBase && addr < d.MmioEnd()
}

// ReadMMIO services a guest read from this device's window. The
// returned value is the little-endian-decoded u32 the guest will
// observe; the caller fills the destination register accordingly.
//
// For offsets in config space (≥ 0x100), `length` may be less than 4
// and we return the requested sub-word.
func (d *VirtioBlk) ReadMMIO(addr uint64, length uint32) uint32 {
	off := uint32(addr - d.mmioBase)
	if off < 0x100 {
		if length != 4 {
			return 0
		}
		v, _ := d.base.ReadCommon(off)
		return v
	}
	// Config space: 16 bytes — capacity (u64), size_max (u32, 0), seg_max (u32, 128).
	var cfg [16]byte
	binary.LittleEndian.PutUint64(cfg[0:8], d.sectors)
	binary.LittleEndian.PutUint32(cfg[8:12], 0)
	binary.LittleEndian.PutUint32(cfg[12:16], 128)
	cfgOff := off - 0x100
	if cfgOff >= uint32(len(cfg)) {
		return 0
	}
	n := length
	if cfgOff+n > uint32(len(cfg)) {
		n = uint32(len(cfg)) - cfgOff
	}
	var ret uint32
	for i := uint32(0); i < n; i++ {
		ret |= uint32(cfg[cfgOff+i]) << (8 * i)
	}
	return ret
}

// WriteMMIO services a guest write to this device's window. Only u32
// writes to common-register offsets are honoured; everything else is
// silently dropped (matches qemu's virtio-mmio behaviour).
func (d *VirtioBlk) WriteMMIO(addr uint64, length uint32, value uint32) {
	off := uint32(addr - d.mmioBase)
	if off >= 0x100 || length != 4 {
		return
	}
	d.base.WriteCommon(off, value, func(queue uint32) {
		// Driver wrote QueueNotify — drain the queue.
		if queue != 0 {
			return
		}
		d.handleQueue()
	})
}

// handleQueue drains the available ring of queue 0, processing each
// request head. Always followed by SignalUsed + raiseIRQ if any
// request was processed.
func (d *VirtioBlk) handleQueue() {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := d.base.CurrentQueue()
	if !q.Ready {
		return
	}
	availIdx, ok := readU16(d.mem, q.DriverAddr+2)
	if !ok {
		return
	}
	processed := false
	for q.LastAvail != availIdx {
		ringOff := q.DriverAddr + 4 + uint64(q.LastAvail%uint16(q.Size))*2
		head, ok := readU16(d.mem, ringOff)
		if !ok {
			break
		}
		q.LastAvail++
		d.processRequest(q, head)
		processed = true
	}
	if processed {
		d.base.SignalUsed()
		if d.raiseIRQ != nil {
			d.raiseIRQ()
		}
	}
}

// processRequest handles a single descriptor chain: 16-byte header,
// zero or more data descriptors, 1-byte status descriptor at the tail.
func (d *VirtioBlk) processRequest(q *VirtioMmioQueue, head uint16) {
	status := virtioBlkSOk
	var written uint32

	chain, err := walkChain(d.mem, q.DescAddr, uint16(q.Size), head)
	if err != nil || len(chain) < 2 {
		// Malformed chain; can't write a status byte. Push used so the
		// driver doesn't stall waiting for this entry.
		pushUsed(d.mem, q.DeviceAddr, uint16(q.Size), uint32(head), 0)
		return
	}
	header := chain[0]
	statusDesc := chain[len(chain)-1]
	if header.Len < 16 || statusDesc.Flags&VirtioDescFWrite == 0 || statusDesc.Len < 1 {
		pushUsed(d.mem, q.DeviceAddr, uint16(q.Size), uint32(head), 0)
		return
	}

	// Read the request header (type, reserved, sector).
	hdrBytes, ok := readBytes(d.mem, header.Addr, 16)
	if !ok {
		pushUsed(d.mem, q.DeviceAddr, uint16(q.Size), uint32(head), 0)
		return
	}
	reqType := binary.LittleEndian.Uint32(hdrBytes[0:4])
	sector := binary.LittleEndian.Uint64(hdrBytes[8:16])

	switch reqType {
	case virtioBlkTIn:
		for i := 1; i < len(chain)-1; i++ {
			desc := chain[i]
			if desc.Flags&VirtioDescFWrite == 0 || desc.Len%512 != 0 {
				status = virtioBlkSIOErr
				break
			}
			if err := d.readDisk(sector, desc.Addr, desc.Len); err != nil {
				status = virtioBlkSIOErr
				break
			}
			written += desc.Len
			sector += uint64(desc.Len) / 512
		}
	case virtioBlkTOut:
		if d.readOnly {
			status = virtioBlkSIOErr
			break
		}
		for i := 1; i < len(chain)-1; i++ {
			desc := chain[i]
			if desc.Flags&VirtioDescFWrite != 0 || desc.Len%512 != 0 {
				status = virtioBlkSIOErr
				break
			}
			if err := d.writeDisk(sector, desc.Addr, desc.Len); err != nil {
				status = virtioBlkSIOErr
				break
			}
			sector += uint64(desc.Len) / 512
		}
	case virtioBlkTFlush:
		if !d.readOnly {
			if err := d.file.Sync(); err != nil {
				status = virtioBlkSIOErr
			}
		}
	case virtioBlkTGetID:
		// Linux/udev reads up to 20 bytes; we publish a fixed ID.
		const id = "gocracker"
		for i := 1; i < len(chain)-1; i++ {
			desc := chain[i]
			n := uint32(len(id))
			if desc.Len < n {
				n = desc.Len
			}
			if !writeBytes(d.mem, desc.Addr, []byte(id)[:n]) {
				status = virtioBlkSIOErr
			}
			written += n
			break // only first data descriptor consumed
		}
	default:
		status = virtioBlkSUnsupp
	}

	// Write the status byte and push the used ring entry. `written + 1`
	// accounts for the status byte the device wrote into the chain.
	_ = writeBytes(d.mem, statusDesc.Addr, []byte{status})
	pushUsed(d.mem, q.DeviceAddr, uint16(q.Size), uint32(head), written+1)
}

// readDisk reads len bytes from the disk at sector*512 into guest RAM
// at gpa. Verifies destination is fully inside guest RAM.
func (d *VirtioBlk) readDisk(sector, gpa uint64, length uint32) error {
	if uint64(length)+gpa > uint64(len(d.mem)) {
		return fmt.Errorf("read GPA %#x+%d out of bounds", gpa, length)
	}
	off := int64(sector * 512)
	_, err := d.file.ReadAt(d.mem[gpa:gpa+uint64(length)], off)
	return err
}

// writeDisk writes len bytes from guest RAM at gpa to the disk at
// sector*512.
func (d *VirtioBlk) writeDisk(sector, gpa uint64, length uint32) error {
	if uint64(length)+gpa > uint64(len(d.mem)) {
		return fmt.Errorf("write GPA %#x+%d out of bounds", gpa, length)
	}
	off := int64(sector * 512)
	_, err := d.file.WriteAt(d.mem[gpa:gpa+uint64(length)], off)
	return err
}

// Close releases the underlying file.
func (d *VirtioBlk) Close() error {
	if d.file != nil {
		return d.file.Close()
	}
	return nil
}

// --- Helpers shared by virtio-mmio devices --------------------------------

// walkChain follows a descriptor chain starting at head and returns the
// list of descriptors in order. Returns an error if the chain has a
// cycle, runs off the end of the descriptor table, or contains an
// unsupported indirect descriptor.
func walkChain(mem []byte, descAddr uint64, qSize uint16, head uint16) ([]VirtqDesc, error) {
	out := make([]VirtqDesc, 0, 4)
	seen := make([]bool, qSize)
	idx := head
	for {
		if idx >= qSize {
			return nil, fmt.Errorf("descriptor index %d out of range (qSize=%d)", idx, qSize)
		}
		if seen[idx] {
			return nil, fmt.Errorf("descriptor cycle at %d", idx)
		}
		seen[idx] = true
		d, ok := ReadVirtqDesc(mem, descAddr+uint64(idx)*16)
		if !ok {
			return nil, fmt.Errorf("descriptor read OOB at index %d", idx)
		}
		if d.Flags&VirtioDescFIndirect != 0 {
			return nil, fmt.Errorf("indirect descriptors not supported")
		}
		out = append(out, d)
		if d.Flags&VirtioDescFNext == 0 {
			return out, nil
		}
		idx = d.Next
	}
}

// pushUsed writes a single entry into the used ring at device_addr and
// bumps the used index. The used ring layout (virtio v1.0):
//
//	struct virtq_used {
//	    le16 flags;
//	    le16 idx;
//	    struct virtq_used_elem ring[];  // { le32 id; le32 len; }
//	};
func pushUsed(mem []byte, deviceAddr uint64, qSize uint16, id, length uint32) {
	idxAddr := deviceAddr + 2
	used, ok := readU16(mem, idxAddr)
	if !ok {
		return
	}
	entry := deviceAddr + 4 + uint64(used%qSize)*8
	if entry+8 > uint64(len(mem)) {
		return
	}
	binary.LittleEndian.PutUint32(mem[entry:entry+4], id)
	binary.LittleEndian.PutUint32(mem[entry+4:entry+8], length)
	binary.LittleEndian.PutUint16(mem[idxAddr:idxAddr+2], used+1)
}

func readU16(mem []byte, addr uint64) (uint16, bool) {
	if addr+2 > uint64(len(mem)) {
		return 0, false
	}
	return binary.LittleEndian.Uint16(mem[addr : addr+2]), true
}

func readBytes(mem []byte, addr uint64, length uint32) ([]byte, bool) {
	if uint64(length)+addr > uint64(len(mem)) {
		return nil, false
	}
	return mem[addr : addr+uint64(length)], true
}

func writeBytes(mem []byte, addr uint64, data []byte) bool {
	if uint64(len(data))+addr > uint64(len(mem)) {
		return false
	}
	copy(mem[addr:addr+uint64(len(data))], data)
	return true
}
