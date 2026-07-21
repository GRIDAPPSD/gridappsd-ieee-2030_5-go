// Package buildinfo holds the bridge binary's version string. Version
// is a package-level var, not a const, so it can be overridden at link
// time via -ldflags:
//
//	go build -ldflags "-X github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/buildinfo.Version=v1.2.3"
//
// The Makefile's build target stamps Version from `git describe`; a
// plain `go build ./...` with no ldflags leaves Version at its
// compiled-in dev sentinel, so a contributor with no make (or on a
// non-tagged checkout) still gets a working binary rather than a
// build failure.
package buildinfo

// Version is the bridge's build version. It defaults to a dev
// sentinel and is overridden by the -X ldflag above when the binary
// is built via `make build`.
var Version = "0.0.0-dev"
