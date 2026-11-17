// Package data embeds the synthetic demo dataset so the binary is
// self-contained and the demo cannot accidentally depend on real records.
package data

import "embed"

//go:embed alerts.json devices.json
var FS embed.FS
