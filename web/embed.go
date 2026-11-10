// Package web embeds the built React UI.
package web

import "embed"

// Dist is the Vite build output. "all:" keeps .gitkeep so a checkout
// without a UI build still compiles.
//
//go:embed all:dist
var Dist embed.FS
