//go:build linux

package virtio

import (
	"encoding/binary"
	"fmt"
	"sync"
	"unsafe"

	gclog "github.com/gocracker/gocracker/internal/log"
	"github.com/gocracker/gocracker/internal/sharedfs"
	"github.com/gocracker/gocracker/internal/vhostuser"
	"golang.org/x/sys/unix"
)

const (
	fsTagBytes          = 36
	fsRequestQueues     = 1
	fsQueueCount        = 1 + fsRequestQueues // hiprio + request queues
	fsSupportedProtoSet = vhostuser.ProtocolFeatureMQ | vhostuser.ProtocolFeatureReplyAck | vhostuser.ProtocolFeatureConfig
)

type FSDevice struct {
	*Transport

	memfd int
	tag   string
	cfg   []byte

	backend          *sharedfs.Backend
	client           *vhostuser.Client
	backendFeatures  uint64
	protocolFeatures uint64
	ackedProtocol    uint64

	mu             sync.Mutex
	activated      bool
	activationErr  error
	callPumpStarted bool
	kickFDs        []int
	callFDs        []int
}

// NewFSDevice creates a virtio-fs MMIO device.
//
// If externalSocket is non-empty the device attaches to an already-listening
// virtiofsd unix socket (used by the worker/jailer path so virtiofsd runs on
// the host). Otherwise it spawns a new virtiofsd against sourceDir.
func NewFSDevice(mem []byte, memfd int, basePA uint64, irq uint8, sourceDir, tag, externalSocket string, dirty *DirtyTracker, irqFn func(bool)) (*FSDevice, error) {
	if tag == "" {
		return nil, fmt.Errorf("virtio-fs tag is required")
	}
	if len(tag) > fsTagBytes {
		return nil, fmt.Errorf("virtio-fs tag %q exceeds %d bytes", tag, fsTagBytes)
	}

	var backend *sharedfs.Backend
	if externalSocket != "" {
		backend = sharedfs.Attach(externalSocket)
	} else {
		var err error
		backend, err = sharedfs.Start(sourceDir, tag)
		if err != nil {
			return nil, fmt.Errorf("start virtiofsd: %w", err)
		}
	}
	client, err := vhostuser.Dial(backend.SocketPath())
	if err != nil {
		_ = backend.Close()
		return nil, fmt.Errorf("connect virtiofsd: %w", err)
	}
	if err := client.SetOwner(); err != nil {
		_ = client.Close()
		_ = backend.Close()
		return nil, fmt.Errorf("vhost-user SET_OWNER: %w", err)
	}
	features, err := client.GetFeatures()
	if err != nil {
		_ = client.Close()
		_ = backend.Close()
		return nil, fmt.Errorf("vhost-user GET_FEATURES: %w", err)
	}
	protoFeatures := uint64(0)
	if features&vhostuser.VhostUserVirtioFeatureProtocolFeatures != 0 {
		protoFeatures, err = client.GetProtocolFeatures()
		if err != nil {
			_ = client.Close()
			_ = backend.Close()
			return nil, fmt.Errorf("vhost-user GET_PROTOCOL_FEATURES: %w", err)
		}
	}

	cfg := make([]byte, fsTagBytes+4)
	copy(cfg[:fsTagBytes], []byte(tag))
	binary.LittleEndian.PutUint32(cfg[fsTagBytes:], fsRequestQueues)

	d := &FSDevice{
		memfd:           memfd,
		tag:             tag,
		cfg:             cfg,
		backend:         backend,
		client:          client,
		backendFeatures: features,
		protocolFeatures: protoFeatures,
	}
	d.Transport = NewTransport(d, mem, basePA, irq, dirty, irqFn)
	return d, nil
}

func (d *FSDevice) DeviceID() uint32       { return DeviceIDFS }
func (d *FSDevice) DeviceFeatures() uint64 { return d.backendFeatures }
func (d *FSDevice) ConfigBytes() []byte    { return append([]byte(nil), d.cfg...) }

func (d *FSDevice) HandleQueue(idx uint32, q *Queue) {}

func (d *FSDevice) HandleQueueNotify(idx uint32, q *Queue) bool {
	d.signalKick(int(idx))
	return false
}

func (d *FSDevice) OnTransportStateChange(t *Transport) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.activated || d.activationErr != nil {
		return
	}
	if !d.transportReadyLocked(t) {
		return
	}
	if err := d.activateLocked(t); err != nil {
		d.activationErr = err
		t.status |= StatusFailed
		gclog.VMM.Error("virtio-fs activation failed", "tag", d.tag, "error", err)
	}
}

func (d *FSDevice) OnTransportReset(t *Transport) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = vhostuser.CloseFDs(append(append([]int{}, d.kickFDs...), d.callFDs...))
	d.kickFDs = nil
	d.callFDs = nil
	d.ackedProtocol = 0
	d.activated = false
	d.activationErr = nil
	d.callPumpStarted = false
}

func (d *FSDevice) RestoreBackendState() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.activated || d.activationErr != nil {
		return d.activationErr
	}
	if !d.transportReadyLocked(d.Transport) {
		return nil
	}
	if err := d.activateLocked(d.Transport); err != nil {
		d.activationErr = err
		return err
	}
	return nil
}

func (d *FSDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	err := vhostuser.CloseFDs(append(append([]int{}, d.kickFDs...), d.callFDs...))
	d.kickFDs = nil
	d.callFDs = nil
	if d.client != nil {
		if closeErr := d.client.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		d.client = nil
	}
	if d.backend != nil {
		if closeErr := d.backend.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		d.backend = nil
	}
	return err
}

func (d *FSDevice) transportReadyLocked(t *Transport) bool {
	if t == nil {
		return false
	}
	requiredStatus := uint32(StatusAcknowledge | StatusDriver | StatusFeaturesOK | StatusDriverOK)
	if t.status&requiredStatus != requiredStatus {
		return false
	}
	for i := 0; i < fsQueueCount; i++ {
		q := t.Queue(i)
		if q == nil || !q.Ready || q.Size == 0 || q.DescAddr == 0 || q.DriverAddr == 0 || q.DeviceAddr == 0 {
			return false
		}
	}
	return true
}

func (d *FSDevice) activateLocked(t *Transport) error {
	if d.client == nil {
		return fmt.Errorf("virtio-fs backend is not connected")
	}
	if d.memfd < 0 {
		return fmt.Errorf("virtio-fs requires memfd-backed guest memory")
	}

	ackedFeatures := t.drvFeatures & d.backendFeatures
	if ackedFeatures&vhostuser.VhostUserVirtioFeatureProtocolFeatures != 0 {
		d.ackedProtocol = d.protocolFeatures & fsSupportedProtoSet
		if err := d.client.SetProtocolFeatures(d.ackedProtocol); err != nil {
			return fmt.Errorf("SET_PROTOCOL_FEATURES: %w", err)
		}
		if d.ackedProtocol&vhostuser.ProtocolFeatureMQ != 0 {
			queueNum, err := d.client.GetQueueNum()
			if err != nil {
				return fmt.Errorf("GET_QUEUE_NUM: %w", err)
			}
			if queueNum < fsQueueCount {
				return fmt.Errorf("virtiofsd only supports %d queues, need %d", queueNum, fsQueueCount)
			}
		}
	}
	if err := d.client.SetFeatures(ackedFeatures); err != nil {
		return fmt.Errorf("SET_FEATURES: %w", err)
	}
	mem := t.Mem()
	if len(mem) == 0 {
		return fmt.Errorf("guest memory is empty")
	}
	region := vhostuser.MemoryRegion{
		GuestPhysAddr: 0,
		MemorySize:    uint64(len(mem)),
		UserAddr:      uint64(uintptr(unsafe.Pointer(&mem[0]))),
		MmapOffset:    0,
	}
	if err := d.client.SetMemTable(region, d.memfd); err != nil {
		return fmt.Errorf("SET_MEM_TABLE: %w", err)
	}
	if err := d.ensureEventFDsLocked(); err != nil {
		return err
	}
	for i := 0; i < fsQueueCount; i++ {
		q := t.Queue(i)
		if err := d.client.SetVringNum(i, uint16(q.Size)); err != nil {
			return fmt.Errorf("SET_VRING_NUM[%d]: %w", i, err)
		}
		addr := vhostuser.VringAddr{
			Descriptor: hostAddr(mem, q.DescAddr),
			Used:       hostAddr(mem, q.DeviceAddr),
			Available:  hostAddr(mem, q.DriverAddr),
		}
		if err := d.client.SetVringAddr(i, addr); err != nil {
			return fmt.Errorf("SET_VRING_ADDR[%d]: %w", i, err)
		}
		if err := d.client.SetVringBase(i, q.LastAvail); err != nil {
			return fmt.Errorf("SET_VRING_BASE[%d]: %w", i, err)
		}
		if err := d.client.SetVringCall(i, d.callFDs[i]); err != nil {
			return fmt.Errorf("SET_VRING_CALL[%d]: %w", i, err)
		}
		if err := d.client.SetVringKick(i, d.kickFDs[i]); err != nil {
			return fmt.Errorf("SET_VRING_KICK[%d]: %w", i, err)
		}
		if ackedFeatures&vhostuser.VhostUserVirtioFeatureProtocolFeatures != 0 {
			if err := d.client.SetVringEnable(i, true); err != nil {
				return fmt.Errorf("SET_VRING_ENABLE[%d]: %w", i, err)
			}
		}
	}
	d.activated = true
	d.startCallPumpsLocked()
	return nil
}

func (d *FSDevice) ensureEventFDsLocked() error {
	if len(d.kickFDs) == fsQueueCount && len(d.callFDs) == fsQueueCount {
		return nil
	}
	newKickFDs := append([]int{}, d.kickFDs...)
	newCallFDs := append([]int{}, d.callFDs...)
	closeNew := func() {
		_ = vhostuser.CloseFDs(newKickFDs[len(d.kickFDs):])
		_ = vhostuser.CloseFDs(newCallFDs[len(d.callFDs):])
	}
	for len(newKickFDs) < fsQueueCount {
		fd, err := unix.Eventfd(0, unix.EFD_CLOEXEC)
		if err != nil {
			closeNew()
			return err
		}
		newKickFDs = append(newKickFDs, fd)
	}
	for len(newCallFDs) < fsQueueCount {
		fd, err := unix.Eventfd(0, unix.EFD_CLOEXEC)
		if err != nil {
			closeNew()
			return err
		}
		newCallFDs = append(newCallFDs, fd)
	}
	d.kickFDs = newKickFDs
	d.callFDs = newCallFDs
	return nil
}

func (d *FSDevice) startCallPumpsLocked() {
	if d.callPumpStarted {
		return
	}
	d.callPumpStarted = true
	for i := range d.callFDs {
		fd := d.callFDs[i]
		go d.callPump(fd)
	}
}

func (d *FSDevice) callPump(fd int) {
	var buf [8]byte
	for {
		_, err := unix.Read(fd, buf[:])
		if err == nil {
			d.Transport.SetInterruptStat(1)
			d.Transport.SignalIRQ(true)
			continue
		}
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		return
	}
}

func (d *FSDevice) signalKick(idx int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if idx < 0 || idx >= len(d.kickFDs) || !d.activated {
		return
	}
	var val [8]byte
	binary.LittleEndian.PutUint64(val[:], 1)
	_, _ = unix.Write(d.kickFDs[idx], val[:])
}

func hostAddr(mem []byte, guestAddr uint64) uint64 {
	return uint64(uintptr(unsafe.Pointer(&mem[guestAddr])))
}
