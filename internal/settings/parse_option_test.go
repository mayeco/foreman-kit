package settings_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
)

// TestParseOptionReadsAnOptionByTheRuleOfItsKey: the value an option of the command line gives for
// a setting goes through the Parse of the setting, so `--interval` and `--set daemon.cycle.every=`
// accept and refuse the same times; the refusal names the option, what was given and the key.
func TestParseOptionReadsAnOptionByTheRuleOfItsKey(t *testing.T) {
	every := settings.Of(settings.CycleEvery)
	for text, want := range map[string]time.Duration{"1m": time.Minute, "2m": 2 * time.Minute, "1h30m": 90 * time.Minute, "90s": 90 * time.Second} {
		got, err := every.ParseOption("--interval", text)
		if err != nil || got != want {
			t.Errorf("ParseOption(%q) = %#v, %v; want %v", text, got, err, want)
		}
		if _, _, err := settings.ParseOverride(string(settings.CycleEvery) + "=" + text); err != nil {
			t.Errorf("--set accepts %q but ParseOption refused it: %v", text, err)
		}
	}
	for _, text := range []string{"30s", "90", "0", "-5m", "", "never", "1.5m"} {
		_, err := every.ParseOption("--interval", text)
		if err == nil {
			t.Errorf("ParseOption(%q) accepted it", text)
			continue
		}
		for _, want := range []string{"--interval", string(settings.CycleEvery), every.Expected()} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ParseOption(%q) says %q, want %q in it", text, err, want)
			}
		}
		if _, _, overrideErr := settings.ParseOverride(string(settings.CycleEvery) + "=" + text); overrideErr == nil && text != "" {
			t.Errorf("ParseOption refused %q but --set accepts it", text)
		}
	}
}
