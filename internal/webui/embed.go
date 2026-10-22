// Package webui embeds the built frontend (web/ → internal/webui/dist).
package webui

import "embed"

//go:embed all:dist
var FS embed.FS
