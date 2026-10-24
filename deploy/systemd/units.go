// Package systemd carries CryoSheep's unit files so `cryosheep install` writes the
// units that match the binary it installs.
package systemd

import "embed"

//go:embed *.service
var Units embed.FS
