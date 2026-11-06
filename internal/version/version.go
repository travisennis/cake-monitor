// Package version reports build identity.
package version

import (
	"fmt"
	"runtime/debug"
)

// Version is the semantic version of this build.
const Version = "0.1.0"

// Revision reports the VCS revision the binary was built from.
func Revision() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	revision := ""
	dirty := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if revision == "" {
		return "", false
	}
	if dirty {
		revision += " (dirty)"
	}
	return revision, true
}

// String is the human-facing build identity.
func String() string {
	revision, ok := Revision()
	if !ok {
		return fmt.Sprintf("cake-monitor %s\nrevision unknown", Version)
	}
	return fmt.Sprintf("cake-monitor %s\nrevision %s", Version, revision)
}
