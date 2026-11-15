package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// TestIntervalTheKeyRefusesStopsTheRun: --interval is the run's value for daemon.cycle.every, so
// the root command and the daemon command read it by the rule of the key: a time with its unit, 1m
// or more. Under a minute, a bare number, zero and negative times exit 2 naming the key, before a
// daemon starts: the command never ends up running a cycle and its judgment agent every 30 seconds.
func TestIntervalTheKeyRefusesStopsTheRun(t *testing.T) {
	root := t.TempDir()
	settingstest.WriteFiles(t, root, nil)
	t.Setenv("FOREMANKIT_APP_ROOT", root)
	for _, args := range [][]string{
		{"--interval", "30s"},
		{"--interval=30s"},
		{"-d", "--interval", "30s"},
		{"-w", "--interval", "30s"},
		{"--interval", "90"},
		{"--interval", "0"},
		{"--interval", "-5m"},
		{"--interval", "soon"},
		{"daemon", "start", "--interval", "90"},
		{"daemon", "start", "--interval=90"},
		{"daemon", "start", "--interval", "30s"},
		{"daemon", "run", "--interval", "30s"},
		{"daemon", "status", "--interval", "90"},
		{"daemon", "start", "--interval", "-5m"},
		{"daemon", "start", "--interval", "0"},
	} {
		t.Setenv(settings.RunOverridesEnv, "")
		var out, errOut bytes.Buffer
		code := Run(context.Background(), args, &out, &errOut)
		if code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), string(settings.CycleEvery)) || !strings.Contains(errOut.String(), "--interval") {
			t.Errorf("%v: exit %d, out %q, err %q; want exit 2 naming --interval and %s", args, code, out.String(), errOut.String(), settings.CycleEvery)
		}
	}
}

// TestIntervalTheKeyAcceptsIsNotRefused: 2m, 1m and 1h30m pass the rule of the key. Outside an
// application the root command prints its help after reading the options, so nothing starts.
func TestIntervalTheKeyAcceptsIsNotRefused(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	t.Chdir(t.TempDir())
	for _, interval := range []string{"2m", "1m", "1h30m", "90s"} {
		for _, args := range [][]string{{"--interval", interval}, {"--interval=" + interval}} {
			t.Setenv(settings.RunOverridesEnv, "")
			var out, errOut bytes.Buffer
			code := Run(context.Background(), args, &out, &errOut)
			if code != 0 || strings.Contains(errOut.String(), "--interval") {
				t.Errorf("%v: exit %d, err %q; want it accepted", args, code, errOut.String())
			}
		}
	}

	root := t.TempDir()
	settingstest.WriteFiles(t, root, nil)
	t.Setenv("FOREMANKIT_APP_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"daemon", "status", "--interval", "2m"}, &out, &errOut); code == 2 {
		t.Errorf("daemon status --interval 2m: exit %d, err %q; want it accepted", code, errOut.String())
	}
}

// TestHelpWritesNoDefaultOfItsOwn: the help of the root command and of the daemon command says each
// default comes from its key of .foreman-kit/daemon.yaml, and writes none of the values the kit
// ships (60m, 4567, 127.0.0.1), which an application that sets the key would see wrong.
func TestHelpWritesNoDefaultOfItsOwn(t *testing.T) {
	t.Setenv(settings.RunOverridesEnv, "")
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"--help"}, {"-h"}, {"daemon", "--help"}, {"daemon", "-h"}} {
		var out, errOut bytes.Buffer
		if code := Run(context.Background(), args, &out, &errOut); code != 0 {
			t.Errorf("%v: exit %d, err %q", args, code, errOut.String())
			continue
		}
		help := out.String() + errOut.String()
		for _, want := range []string{
			"--interval", "--port", "--bind",
			settings.CycleEvery.Where(), settings.DaemonPort.Where(), settings.DaemonBind.Where(),
		} {
			if !strings.Contains(help, want) {
				t.Errorf("%v: the help lacks %q:\n%s", args, want, help)
			}
		}
		for _, key := range []settings.Key{settings.CycleEvery, settings.DaemonPort, settings.DaemonBind} {
			shipped := fmt.Sprint(settings.Of(key).Default)
			for _, literal := range []string{"(default " + shipped + ")", "--interval " + shipped, "--port " + shipped, "--bind " + shipped} {
				if strings.Contains(help, literal) {
					t.Errorf("%v: the help writes the shipped default by hand (%q):\n%s", args, literal, help)
				}
			}
		}
	}
}
