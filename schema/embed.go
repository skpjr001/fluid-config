// Package schema holds the JSON Schemas for the config files.
package schema

import "embed"

// FS holds every *.schema.json file.
//
//go:embed *.json
var FS embed.FS
