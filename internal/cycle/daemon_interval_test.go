package cycle

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
)

// TestParseIntervalReadsWhatTheKeyAccepts: --interval is the run's value for daemon.cycle.every and
// takes the rule of the key: a time with its unit, 1m or more. A bare number has no unit, zero and
// negative times are not times, and what is refused names the option, the value and the key.
func TestParseIntervalReadsWhatTheKeyAccepts(t *testing.T) {
	for text, want := range map[string]time.Duration{
		"1m":    time.Minute,
		"2m":    2 * time.Minute,
		"90s":   90 * time.Second,
		"1h30m": 90 * time.Minute,
		" 5m ":  5 * time.Minute,
	} {
		got, err := ParseInterval(text)
		if err != nil || got != want {
			t.Errorf("ParseInterval(%q) = %v, %v; want %v", text, got, err, want)
		}
	}
	for _, text := range []string{"30s", "59s", "90", "0", "0s", "-5m", "abc", "", "1.5m", "never"} {
		got, err := ParseInterval(text)
		if err == nil || got != 0 {
			t.Errorf("ParseInterval(%q) = %v, %v; want a refusal", text, got, err)
			continue
		}
		for _, want := range []string{"--interval", string(settings.CycleEvery), "1m or more"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ParseInterval(%q) says %q, want %q in it", text, err, want)
			}
		}
	}
}

// TestDaemonCLIRefusesAnIntervalTheKeyRefuses: every form of --interval of `foreman-kit daemon`, in
// every subcommand, goes through the key's rule and exits 2 before anything starts. A bare 90 is not
// 90 minutes; 30s is under the minimum; zero and negative values are not ignored.
func TestDaemonCLIRefusesAnIntervalTheKeyRefuses(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	root := bareApp(t)
	st := openTestStore(t, root)
	for _, args := range [][]string{
		{"start", "--interval", "90"},
		{"start", "--interval=90"},
		{"start", "--interval", "30s"},
		{"run", "--interval=30s"},
		{"status", "--interval", "90"},
		{"stop", "--interval=0"},
		{"restart", "--interval", "-5m"},
		{"--interval", "0s"},
		{"start", "--interval", "soon"},
	} {
		var out, errOut bytes.Buffer
		rc := RunDaemonCLI(context.Background(), root, st, DaemonOptions{}, args, &out, &errOut)
		if rc != 2 || out.Len() != 0 {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want exit 2 and nothing started", args, rc, out.String(), errOut.String())
			continue
		}
		for _, want := range []string{"daemon: ", "--interval", string(settings.CycleEvery)} {
			if !strings.Contains(errOut.String(), want) {
				t.Errorf("%v: stderr %q, want %q in it", args, errOut.String(), want)
			}
		}
	}
}

// TestDaemonCLIAcceptsAnIntervalTheKeyAccepts: `daemon status --interval 2m` is not refused.
func TestDaemonCLIAcceptsAnIntervalTheKeyAccepts(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	root := bareApp(t)
	st := openTestStore(t, root)
	for _, args := range [][]string{{"status", "--interval", "2m"}, {"status", "--interval=1m30s"}} {
		var out, errOut bytes.Buffer
		if rc := RunDaemonCLI(context.Background(), root, st, DaemonOptions{}, args, &out, &errOut); rc == 2 || strings.Contains(errOut.String(), "--interval") {
			t.Errorf("%v: exit %d, stderr %q; want it accepted", args, rc, errOut.String())
		}
	}
}

// TestTheDetachedDaemonIsHandedAnIntervalItsOwnParserAccepts: the daemon that start detaches is run
// again as `daemon run --interval <time>`; what is passed, from the key's value or from --interval,
// must be read back by the same rule, whatever the kit's default is.
func TestTheDetachedDaemonIsHandedAnIntervalItsOwnParserAccepts(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	for name, opts := range map[string]DaemonOptions{
		"the interval of the key": {Detach: true},
		"90s from --interval":     {Detach: true, Interval: 90 * time.Second},
		"2h from --interval":      {Detach: true, Interval: 2 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "argv")
			t.Setenv("DETACHED_SPAWN_RECORD", record)
			detachedSpawnStandIn(t, `{ printf '%s\n' "$@"; } > "$DETACHED_SPAWN_RECORD.tmp" && mv "$DETACHED_SPAWN_RECORD.tmp" "$DETACHED_SPAWN_RECORD"; exec sleep 60`)
			root := bareApp(t)
			st := openTestStore(t, root)
			var out, errOut bytes.Buffer
			rc := StartDaemon(context.Background(), root, st, opts, &out, &errOut)
			if m := detachedSpawnPIDRe.FindStringSubmatch(out.String() + errOut.String()); m != nil {
				// Stop the stand-in this test started.
				if pid, err := strconv.Atoi(m[1]); err == nil && pid > 1 {
					defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
				}
			}
			if rc != 0 {
				t.Fatalf("StartDaemon exit %d: %s %s", rc, out.String(), errOut.String())
			}
			var argv []string
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if data, err := os.ReadFile(record); err == nil {
					argv = strings.Fields(string(data))
					break
				}
			}
			given := ""
			for i, a := range argv {
				if a == "--interval" && i+1 < len(argv) {
					given = argv[i+1]
				}
			}
			if given == "" {
				t.Fatalf("the detached daemon got no --interval: %v", argv)
			}
			d, err := ParseInterval(given)
			if err != nil {
				t.Fatalf("the detached daemon was handed --interval %s, which its own parser refuses: %v", given, err)
			}
			if opts.Interval > 0 && d != opts.Interval {
				t.Errorf("handed %s, want %s", d, opts.Interval)
			}
		})
	}
}

// TestDaemonHelpTakesItsDefaultsFromTheCatalog: the help of the daemon writes no default of its own:
// each comes from the key that holds it, so an application that changes one is not told the kit's.
func TestDaemonHelpTakesItsDefaultsFromTheCatalog(t *testing.T) {
	help := DaemonHelp() + "\n" + DaemonUsage
	for _, want := range []string{
		settings.CycleEvery.Where(), settings.DaemonPort.Where(), settings.DaemonBind.Where(),
		settings.Of(settings.CycleEvery).Expected(), "--interval", "--port", "--bind",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("the daemon help lacks %q:\n%s", want, help)
		}
	}
	for _, shipped := range []any{settings.Of(settings.CycleEvery).Default, settings.Of(settings.DaemonPort).Default, settings.Of(settings.DaemonBind).Default} {
		if text := fmt.Sprint(shipped); strings.Contains(help, text) {
			t.Errorf("the daemon help writes the shipped value %q by hand:\n%s", text, help)
		}
	}
}
