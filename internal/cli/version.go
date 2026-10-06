package cli

import "runtime/debug"

// Version is set at release time with -ldflags "-X github.com/marvell/sprut/internal/cli.Version=...".
var Version string

func version() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}
