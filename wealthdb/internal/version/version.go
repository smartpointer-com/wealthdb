// Package version exposes the build version of the wealthdb binary.
package version

// Version is the build version. Default is "dev"; release builds
// override at link time:
//
//	go build -ldflags '-X github.com/ptu/wealthdb/internal/version.Version=v0.1.0'
var Version = "dev"
