//go:build amd64

package embed

import _ "embed"

//go:embed gocracker-toolbox-amd64
var Binary []byte
