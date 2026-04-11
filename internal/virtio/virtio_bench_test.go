package virtio

import (
	"encoding/binary"
	"math/rand"
	"testing"
)

// setupMockQueue creates a queue populated with 256 descriptors ready to consume.
func setupMockQueue(b *testing.B) (*Queue, []byte) {
	mem := make([]byte, 1024*1024)
	q := NewQueue(mem, 256, nil)
	
	q.DescAddr = 0x0
	q.DriverAddr = 0x1000
	q.DeviceAddr = 0x2000
	q.guestPhysBase = 0

	// Set Avail.Idx = 256 effectively meaning 256 completed requests
	binary.LittleEndian.PutUint16(mem[0x1002:0x1004], 256)
	
	// Fill Avail.Ring
	for i := 0; i < 256; i++ {
		binary.LittleEndian.PutUint16(mem[0x1004+2*i:0x1004+2*i+2], uint16(i))
	}

	return q, mem
}

func BenchmarkQueueConsumeAndPush(b *testing.B) {
	q, mem := setupMockQueue(b)
	
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			consumed, _ := q.ConsumeAvail(func(head uint16) {
				// simulate work
			})
			if !consumed {
				// reset simulation
				q.mu.Lock()
				q.LastAvail = 0
				// Reset Used.Idx
				binary.LittleEndian.PutUint16(mem[0x2002:0x2004], 0)
				q.mu.Unlock()
			} else {
				q.PushUsed(uint32(rand.Intn(256)), 0)
			}
		}
	})
}
