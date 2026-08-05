// Package version reports the build identity of the binary.
package version

import "runtime/debug"

// version is set at build time with
// -ldflags "-X github.com/mtrenker/pacapulse/internal/version.version=<sha>".
// Container images are tagged with the same commit SHA, so a running pod can
// be traced back to the source it was built from.
var version = ""

// Version returns the build identity: the injected value, or the VCS revision
// recorded by the Go toolchain, or "dev".
func Version() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				return setting.Value
			}
		}
	}
	return "dev"
}
