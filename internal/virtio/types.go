// This file contains pure data type definitions used for snapshot serialization.
// These types have no platform-specific dependencies and compile on all targets.
package virtio

// QueueState stores the serializable state of a single virtqueue.
type QueueState struct {
	Size       uint32 `json:"size"`
	Ready      bool   `json:"ready"`
	LastAvail  uint16 `json:"last_avail"`
	DescAddr   uint64 `json:"desc_addr"`
	DriverAddr uint64 `json:"driver_addr"`
	DeviceAddr uint64 `json:"device_addr"`
}

// TransportState stores the serializable state of a virtio MMIO transport.
type TransportState struct {
	Status         uint32       `json:"status"`
	DrvFeatures    uint64       `json:"drv_features"`
	DevFeaturesSel uint32       `json:"dev_features_sel"`
	DrvFeaturesSel uint32       `json:"drv_features_sel"`
	QueueSel       uint32       `json:"queue_sel"`
	InterruptStat  uint32       `json:"interrupt_stat"`
	Queues         []QueueState `json:"queues"`
}
