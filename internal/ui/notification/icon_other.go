//go:build !darwin

package notification

import (
	_ "embed"
)

//go:embed trapeze-icon-solo.png
var Icon []byte
