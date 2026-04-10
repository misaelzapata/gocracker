package vz

/*
#cgo darwin CFLAGS: -mmacosx-version-min=11 -x objective-c -fno-objc-arc
#cgo darwin LDFLAGS: -lobjc -framework Foundation -framework Virtualization -framework vmnet
# include "virtualization_26.h"
# include <CoreFoundation/CoreFoundation.h>
# include <vmnet/vmnet.h>

static inline void cfReleasePtr(void *ptr) {
	if (ptr != NULL) {
		CFRelease(ptr);
	}
}
*/
import "C"
import (
	"fmt"
	"net"
	"runtime"
	"unsafe"

	"github.com/Code-Hex/vz/v3/internal/objc"
)

type VmnetMode uint32

const (
	VmnetModeHost   VmnetMode = VmnetMode(C.VMNET_HOST_MODE)
	VmnetModeShared VmnetMode = VmnetMode(C.VMNET_SHARED_MODE)
)

type VmnetNetworkConfiguration struct {
	ptr unsafe.Pointer
}

func NewVmnetNetworkConfiguration(mode VmnetMode) (*VmnetNetworkConfiguration, error) {
	if err := macOSAvailable(26); err != nil {
		return nil, err
	}
	var status C.vmnet_return_t
	ptr := C.vmnet_network_configuration_create(C.vmnet_mode_t(mode), &status)
	if ptr == nil {
		return nil, fmt.Errorf("create vmnet network configuration: %s", vmnetStatusString(status))
	}
	cfg := &VmnetNetworkConfiguration{ptr: unsafe.Pointer(ptr)}
	runtime.SetFinalizer(cfg, func(self *VmnetNetworkConfiguration) {
		C.cfReleasePtr(self.ptr)
	})
	return cfg, nil
}

func (c *VmnetNetworkConfiguration) SetIPv4Subnet(subnet *net.IPNet) error {
	if c == nil || c.ptr == nil {
		return fmt.Errorf("vmnet network configuration is nil")
	}
	if subnet == nil {
		return fmt.Errorf("vmnet subnet is nil")
	}
	ip4 := subnet.IP.To4()
	if ip4 == nil {
		return fmt.Errorf("vmnet subnet must be IPv4")
	}
	mask := net.IP(subnet.Mask).To4()
	if mask == nil {
		return fmt.Errorf("vmnet subnet mask must be IPv4")
	}
	var subnetAddr C.struct_in_addr
	copy((*[4]byte)(unsafe.Pointer(&subnetAddr))[:], ip4)
	var subnetMask C.struct_in_addr
	copy((*[4]byte)(unsafe.Pointer(&subnetMask))[:], mask)
	status := C.vmnet_network_configuration_set_ipv4_subnet(
		(C.vmnet_network_configuration_ref)(c.ptr),
		&subnetAddr,
		&subnetMask,
	)
	if status != C.VMNET_SUCCESS {
		return fmt.Errorf("set vmnet ipv4 subnet %s: %s", subnet, vmnetStatusString(status))
	}
	return nil
}

func (c *VmnetNetworkConfiguration) DisableDHCP() {
	if c == nil || c.ptr == nil {
		return
	}
	C.vmnet_network_configuration_disable_dhcp((C.vmnet_network_configuration_ref)(c.ptr))
}

func (c *VmnetNetworkConfiguration) DisableNAT44() {
	if c == nil || c.ptr == nil {
		return
	}
	C.vmnet_network_configuration_disable_nat44((C.vmnet_network_configuration_ref)(c.ptr))
}

func (c *VmnetNetworkConfiguration) DisableDNSProxy() {
	if c == nil || c.ptr == nil {
		return
	}
	C.vmnet_network_configuration_disable_dns_proxy((C.vmnet_network_configuration_ref)(c.ptr))
}

func (c *VmnetNetworkConfiguration) SetExternalInterface(name string) error {
	if c == nil || c.ptr == nil {
		return fmt.Errorf("vmnet network configuration is nil")
	}
	cs := charWithGoString(name)
	defer cs.Free()
	status := C.vmnet_network_configuration_set_external_interface((C.vmnet_network_configuration_ref)(c.ptr), cs.CString())
	if status != C.VMNET_SUCCESS {
		return fmt.Errorf("set vmnet external interface %q: %s", name, vmnetStatusString(status))
	}
	return nil
}

type VmnetNetwork struct {
	ptr unsafe.Pointer
}

func NewVmnetNetwork(cfg *VmnetNetworkConfiguration) (*VmnetNetwork, error) {
	if err := macOSAvailable(26); err != nil {
		return nil, err
	}
	if cfg == nil || cfg.ptr == nil {
		return nil, fmt.Errorf("vmnet network configuration is nil")
	}
	var status C.vmnet_return_t
	ptr := C.vmnet_network_create((C.vmnet_network_configuration_ref)(cfg.ptr), &status)
	if ptr == nil {
		return nil, fmt.Errorf("create vmnet network: %s", vmnetStatusString(status))
	}
	network := &VmnetNetwork{ptr: unsafe.Pointer(ptr)}
	runtime.SetFinalizer(network, func(self *VmnetNetwork) {
		C.cfReleasePtr(self.ptr)
	})
	return network, nil
}

func (n *VmnetNetwork) ptrValue() unsafe.Pointer {
	if n == nil {
		return nil
	}
	return n.ptr
}

func (n *VmnetNetwork) IPv4Subnet() (*net.IPNet, error) {
	if n == nil || n.ptr == nil {
		return nil, fmt.Errorf("vmnet network is nil")
	}
	var subnet C.struct_in_addr
	var mask C.struct_in_addr
	C.vmnet_network_get_ipv4_subnet((C.vmnet_network_ref)(n.ptr), &subnet, &mask)
	subnetBytes := *(*[4]byte)(unsafe.Pointer(&subnet))
	maskBytes := *(*[4]byte)(unsafe.Pointer(&mask))
	subnetIP := net.IPv4(subnetBytes[0], subnetBytes[1], subnetBytes[2], subnetBytes[3]).To4()
	maskIP := net.IPv4(maskBytes[0], maskBytes[1], maskBytes[2], maskBytes[3]).To4()
	if subnetIP == nil || maskIP == nil {
		return nil, fmt.Errorf("vmnet network did not return an IPv4 subnet")
	}
	return &net.IPNet{IP: subnetIP, Mask: net.IPMask(maskIP)}, nil
}

type VmnetNetworkDeviceAttachment struct {
	*pointer

	*baseNetworkDeviceAttachment

	network *VmnetNetwork
}

func (*VmnetNetworkDeviceAttachment) String() string {
	return "VmnetNetworkDeviceAttachment"
}

var _ NetworkDeviceAttachment = (*VmnetNetworkDeviceAttachment)(nil)

func NewVmnetNetworkDeviceAttachment(network *VmnetNetwork) (*VmnetNetworkDeviceAttachment, error) {
	if err := macOSAvailable(26); err != nil {
		return nil, err
	}
	if network == nil || network.ptr == nil {
		return nil, fmt.Errorf("vmnet network is nil")
	}
	attachment := &VmnetNetworkDeviceAttachment{
		pointer: objc.NewPointer(C.newVZVmnetNetworkDeviceAttachment(network.ptrValue())),
		network: network,
	}
	objc.SetFinalizer(attachment, func(self *VmnetNetworkDeviceAttachment) {
		objc.Release(self)
	})
	return attachment, nil
}

func vmnetStatusString(status C.vmnet_return_t) string {
	switch status {
	case C.VMNET_SUCCESS:
		return "success"
	case C.VMNET_FAILURE:
		return "failure"
	case C.VMNET_MEM_FAILURE:
		return "memory failure"
	case C.VMNET_INVALID_ARGUMENT:
		return "invalid argument"
	case C.VMNET_SETUP_INCOMPLETE:
		return "setup incomplete"
	case C.VMNET_INVALID_ACCESS:
		return "invalid access"
	case C.VMNET_PACKET_TOO_BIG:
		return "packet too big"
	case C.VMNET_BUFFER_EXHAUSTED:
		return "buffer exhausted"
	case C.VMNET_TOO_MANY_PACKETS:
		return "too many packets"
	case C.VMNET_SHARING_SERVICE_BUSY:
		return "sharing service busy"
	case C.VMNET_NOT_AUTHORIZED:
		return "not authorized"
	default:
		return fmt.Sprintf("vmnet status %d", int(status))
	}
}
