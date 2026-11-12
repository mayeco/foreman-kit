package assets

import (
	"runtime/debug"
	"strings"
)

// The values a binary carries when nothing was injected with -ldflags.
const (
	defaultVersion      = "4.0.0-dev"
	defaultSourceCommit = "HEAD"
	defaultBuiltAt      = ""
)

func init() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	fillFromBuildInfo(info)
}

// fillFromBuildInfo fills what -ldflags left unset from the build information Go stamps into a
// binary built inside the git checkout: the commit and its time, and the module version when Go
// derived a real one from a tag. The module path has no /v4 suffix, so Go mostly stamps a
// pseudo-version v0.0.0-<time>-<commit>, which says nothing of the release and is ignored: the
// version then comes from scripts/build.sh (scripts/version.sh), or stays the default.
func fillFromBuildInfo(info *debug.BuildInfo) {
	var revision, revTime string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			revTime = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if Version == defaultVersion {
		v := strings.TrimSuffix(strings.TrimPrefix(info.Main.Version, "v"), "+incompatible")
		if v != "" && v != "(devel)" && !strings.HasPrefix(v, "0.0.0-") {
			Version = v
		}
	}
	if SourceCommit == defaultSourceCommit && revision != "" {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		if dirty {
			revision += "-dirty"
		}
		SourceCommit = revision
	}
	if BuiltAt == defaultBuiltAt && revTime != "" {
		BuiltAt = revTime
	}
}
