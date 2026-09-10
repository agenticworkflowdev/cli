// Package version provides the AWDev CLI's build version.
package version

import "runtime/debug"

// Version is the release version embedded in the binary.
//
// Release builds replace this value with a linker flag. Keeping a development
// default makes local builds and go run output explicit instead of pretending
// to be a released version.
var Version = "dev"

// String returns the release version, falling back to the main module version
// embedded by the Go toolchain when available.
func String() string {
	if Version != "" && Version != "dev" {
		return Version
	}

	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		if buildInfo.Main.Version != "" && buildInfo.Main.Version != "(devel)" {
			return buildInfo.Main.Version
		}
	}

	return Version
}
