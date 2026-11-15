package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/portal"
	"github.com/mayeco/foreman-kit/internal/settings"
)

// TestPortalHelpListsItsFlagsOutsideAnApplication: `foreman-kit portal --help` and `-h` exit 0
// and print the portal's own help, with both flags, the range of a port and the settings each
// flag stands in for, from a directory that is not an application umbrella.
func TestPortalHelpListsItsFlagsOutsideAnApplication(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	t.Setenv("FOREMANKIT_APP_ROOT", "")
	t.Setenv("FOREMANKIT_DB_PATH", "")
	t.Chdir(t.TempDir())

	var helps []string
	for _, flag := range []string{"--help", "-h"} {
		var out, errOut bytes.Buffer
		code := Run(context.Background(), []string{"portal", flag}, &out, &errOut)
		if code != 0 {
			t.Fatalf("portal %s exited %d: %s %s", flag, code, out.String(), errOut.String())
		}
		for _, want := range []string{"--port", "-p", "--bind", "-o", "1 to 65535", settings.DaemonPort.Path(), settings.DaemonBind.Path(), portal.PortalUsage} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("portal %s printed %q, want %q in it", flag, out.String(), want)
			}
		}
		if strings.Contains(out.String()+errOut.String(), "listening on") {
			t.Errorf("portal %s started a portal: %q %q", flag, out.String(), errOut.String())
		}
		helps = append(helps, out.String())
	}
	if helps[0] != helps[1] {
		t.Errorf("portal --help and -h differ:\n%s\n---\n%s", helps[0], helps[1])
	}
}
