package hostguard

// DeviceRequirements specifies which host devices are needed by a VM.
type DeviceRequirements struct {
	NeedKVM bool
	NeedTun bool
}
