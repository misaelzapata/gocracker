//go:build darwin

package hostnet

// AutoNetwork manages host-side TAP networking for a VM.
// On macOS, TAP networking is not available; use vz NAT mode instead.
type AutoNetwork struct{}

// NewAuto creates an auto-configured network for a VM.
// Returns ErrTAPNotSupported on macOS since TAP devices are not available.
func NewAuto(project, tapName string) (*AutoNetwork, error) {
	return nil, errTAPNotAvailable
}

func (a *AutoNetwork) TapName() string          { return "" }
func (a *AutoNetwork) GuestCIDR() string        { return "" }
func (a *AutoNetwork) GuestIP() string           { return "" }
func (a *AutoNetwork) GatewayIP() string         { return "" }
func (a *AutoNetwork) UpstreamInterface() string { return "" }
func (a *AutoNetwork) Activate() error           { return errTAPNotAvailable }
func (a *AutoNetwork) Close()                    {}

var errTAPNotAvailable = &tapNotAvailableError{}

type tapNotAvailableError struct{}

func (e *tapNotAvailableError) Error() string {
	return "TAP networking is not available on macOS; use NAT mode"
}
