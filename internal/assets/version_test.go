package assets

import (
	"runtime/debug"
	"testing"
)

func TestFillFromBuildInfo(t *testing.T) {
	saved := [3]string{Version, SourceCommit, BuiltAt}
	t.Cleanup(func() { Version, SourceCommit, BuiltAt = saved[0], saved[1], saved[2] })

	Version, SourceCommit, BuiltAt = defaultVersion, defaultSourceCommit, defaultBuiltAt
	fillFromBuildInfo(&debug.BuildInfo{
		Main: debug.Module{Version: "v4.0.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef0123"},
			{Key: "vcs.time", Value: "2026-11-11T00:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	})
	if Version != "4.0.0" || SourceCommit != "0123456789ab-dirty" || BuiltAt != "2026-11-11T00:00:00Z" {
		t.Fatalf("got %q %q %q", Version, SourceCommit, BuiltAt)
	}

	// What -ldflags injected is kept.
	Version, SourceCommit, BuiltAt = "4.1.0", "abc", "then"
	fillFromBuildInfo(&debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "ffff"}}})
	if Version != "4.1.0" || SourceCommit != "abc" || BuiltAt != "then" {
		t.Fatalf("ldflags values overwritten: %q %q %q", Version, SourceCommit, BuiltAt)
	}

	// A build without a usable tag keeps the default.
	for _, v := range []string{"(devel)", "", "v0.0.0-20261111232625-074a7b4a38b2+dirty"} {
		Version = defaultVersion
		fillFromBuildInfo(&debug.BuildInfo{Main: debug.Module{Version: v}})
		if Version != defaultVersion {
			t.Fatalf("%q: Version = %q", v, Version)
		}
	}
}
