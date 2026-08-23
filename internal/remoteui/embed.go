// Package remoteui contains MoHuddle's embedded, mobile-friendly remote client.
package remoteui

import (
	"embed"
	"io/fs"
)

//go:embed static/*
var embedded embed.FS

// FS returns a fresh view rooted at the web application directory.
func FS() fs.FS {
	assets, err := fs.Sub(embedded, "static")
	if err != nil {
		panic("remote UI assets are missing: " + err.Error())
	}
	return assets
}
