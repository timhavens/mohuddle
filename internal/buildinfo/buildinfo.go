// Package buildinfo carries build-time identity that both the command layer
// and the terminal UI need. It lives outside package main so the UI can render
// the running version without importing the command binary.
package buildinfo

// Version is the MoHuddle build version. Release builds overwrite it through
// -ldflags "-X github.com/timhavens/mohuddle/internal/buildinfo.Version=...".
// A plain `go build` leaves the placeholder in place.
var Version = "dev"
