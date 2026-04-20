//go:build arm64

package embed

import _ "embed"

//go:embed gocracker-toolbox-arm64
var Binary []byte
