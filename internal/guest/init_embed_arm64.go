//go:build arm64

package guest

import _ "embed"

// embeddedInit contains the pre-compiled guest init binary for linux/arm64.
//
//go:embed init_arm64.bin
var embeddedInit []byte
