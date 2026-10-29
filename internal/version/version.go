// Package version reports the chainmail build version.
package version

// Version is set at build time with
// -ldflags "-X github.com/jamesainslie/chainmail/internal/version.Version=<v>".
var Version = "dev"
