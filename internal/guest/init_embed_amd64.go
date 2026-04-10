//go:build amd64

package guest

import _ "embed"

// embeddedInit contains the pre-compiled guest init binary for linux/amd64.
//
//go:embed init_amd64.bin
var embeddedInit []byte
