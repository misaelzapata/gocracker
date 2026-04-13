package vmm

import "fmt"

func copyGuestPayload(mem []byte, guestMemBase, guestAddr uint64, payload []byte) error {
	if guestAddr < guestMemBase {
		return fmt.Errorf("guest address %#x precedes guest memory base %#x", guestAddr, guestMemBase)
	}
	offset := guestAddr - guestMemBase
	if offset > uint64(len(mem)) || offset+uint64(len(payload)) > uint64(len(mem)) {
		return fmt.Errorf("payload at %#x (%d bytes) exceeds guest RAM", guestAddr, len(payload))
	}
	copy(mem[offset:offset+uint64(len(payload))], payload)
	return nil
}
