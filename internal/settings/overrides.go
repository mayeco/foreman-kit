package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// RunOverridesEnv is the variable that carries the values `foreman-kit --set` gave for one run: to
// the process itself and to what it starts (the daemon it launches, the gates an agent runs in its
// worktree). It is context the kit passes, like FOREMANKIT_SLOT: nobody writes it by hand, and it is
// not a setting.
const RunOverridesEnv = "FOREMANKIT_SET"

// ParseOverride reads one argument of `--set`, "<file>.<key>=<value>", the way `foreman-kit config
// set` reads its own: the key is one of the catalog, named with its file (pipeline.dispatch.workers),
// and the value is written as the file would write it (3, 30m, never, true, [claude, codex]). It
// returns the key and the value as text, or why the argument cannot be used.
func ParseOverride(arg string) (Key, string, error) {
	name, text, hasValue := strings.Cut(arg, "=")
	name, text = strings.TrimSpace(name), strings.TrimSpace(text)
	if !hasValue || name == "" {
		return "", "", fmt.Errorf("--set takes <file>.<key>=<value>, for example --set %s=3, not %q", Workers, arg)
	}
	s, known := Lookup(Key(name))
	if !known {
		return "", "", fmt.Errorf("--set %s: not a setting (foreman-kit config show lists them, each with its file: %s)", name, Workers)
	}
	if !s.Settable() {
		return "", "", fmt.Errorf("--set %s: it is not one value, and --set gives one: %s", name, s.Change)
	}
	raw, err := overrideValue(text)
	if err != nil {
		return "", "", fmt.Errorf("--set %s: %q is not a value: %v", name, text, err)
	}
	if raw == nil {
		return "", "", fmt.Errorf("--set %s: it has no value", name)
	}
	if problem := s.Problem(raw); problem != "" {
		return "", "", fmt.Errorf("--set %s: %s (is %s)", name, problem, Inline(raw))
	}
	return s.Key, text, nil
}

// overrideValue reads the text of a value the way its file would hold it.
func overrideValue(text string) (any, error) {
	var raw any
	if err := yaml.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("%s", firstLine(err.Error()))
	}
	return raw, nil
}

// EncodeOverrides writes the values of one run for RunOverridesEnv. values maps each key to the text
// ParseOverride returned for it.
func EncodeOverrides(values map[Key]string) string {
	if len(values) == 0 {
		return ""
	}
	plain := make(map[string]string, len(values))
	for key, text := range values {
		plain[string(key)] = text
	}
	out, err := json.Marshal(plain)
	if err != nil {
		return ""
	}
	return string(out)
}

// DecodeOverrides reads what EncodeOverrides wrote. What `--set` would not have accepted holds
// nothing: a value nobody validated is not read (RunOverridesProblem says what it was).
func DecodeOverrides(text string) map[Key]string {
	values, _ := decodeOverrides(text)
	return values
}

// decodeOverrides returns the values text holds, and the first thing in it, by key, that `--set`
// would not have accepted.
func decodeOverrides(text string) (map[Key]string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	var plain map[string]string
	if err := json.Unmarshal([]byte(text), &plain); err != nil {
		return nil, fmt.Errorf("it is not a list of values")
	}
	names := make([]string, 0, len(plain))
	for name := range plain {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(map[Key]string, len(plain))
	var first error
	for _, name := range names {
		key, value, err := ParseOverride(name + "=" + plain[name])
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		out[key] = value
	}
	return out, first
}

// RunOverridesProblem says why what this process holds in RunOverridesEnv cannot be used: it is
// not what `foreman-kit --set` writes there. A process that reads it anyway would run on other
// values than the ones it was given, so `foreman-kit` does not start with it.
func RunOverridesProblem() error {
	if _, err := decodeOverrides(os.Getenv(RunOverridesEnv)); err != nil {
		return fmt.Errorf("%s holds what foreman-kit --set did not write (%v): unset it, and give the values of this run with --set", RunOverridesEnv, err)
	}
	return nil
}

// RunOverrides returns the values `foreman-kit --set` gave this run, sorted by key, each as
// "<file>.<key>=<value>": what a process says it runs with.
func RunOverrides() []string {
	values := DecodeOverrides(os.Getenv(RunOverridesEnv))
	out := make([]string, 0, len(values))
	for key, text := range values {
		out = append(out, string(key)+"="+text)
	}
	sort.Strings(out)
	return out
}

// overrideText returns the value `--set` gave key for this run, as it was written.
func (r *Reader) overrideText(key Key) (string, bool) {
	if !r.overridesRead {
		r.overridesRead = true
		if !r.FilesAlone {
			r.overrides = DecodeOverrides(os.Getenv(RunOverridesEnv))
		}
	}
	text, given := r.overrides[key]
	return text, given
}

// override returns the value `--set` gave key for this run, as its file would hold it.
func (r *Reader) override(key Key) (any, bool) {
	text, given := r.overrideText(key)
	if !given {
		return nil, false
	}
	raw, err := overrideValue(text)
	if err != nil || raw == nil {
		return nil, false
	}
	return raw, true
}

// GiveForThisRun gives values to this process, and to what it starts, the way `--set` does: each
// key of values reads as its text from now on, over what the process was already given. It returns
// what puts things back as they were. A value the setting does not accept is refused and nothing
// changes.
func GiveForThisRun(values map[Key]string) (restore func(), err error) {
	before, had := os.LookupEnv(RunOverridesEnv)
	given := DecodeOverrides(before)
	if given == nil {
		given = map[Key]string{}
	}
	for key, text := range values {
		checked, _, err := ParseOverride(string(key) + "=" + text)
		if err != nil {
			return func() {}, err
		}
		given[checked] = text
	}
	if err := os.Setenv(RunOverridesEnv, EncodeOverrides(given)); err != nil {
		return func() {}, err
	}
	return func() {
		if had {
			_ = os.Setenv(RunOverridesEnv, before)
		} else {
			_ = os.Unsetenv(RunOverridesEnv)
		}
	}, nil
}

// GivenForThisRun returns the value `--set` gave key for this run, as its file would hold it, read
// when it is asked: a process that was given one after it loaded its settings still reads it.
func GivenForThisRun(key Key) (any, bool) {
	text, given := DecodeOverrides(os.Getenv(RunOverridesEnv))[key]
	if !given {
		return nil, false
	}
	raw, err := overrideValue(text)
	if err != nil || raw == nil {
		return nil, false
	}
	return raw, true
}

// EvaluatorOffForThisRun reports whether this run was given evaluator.enabled=false with `--set`:
// every gate of the evaluator is then off for it, whatever the file says.
func EvaluatorOffForThisRun() bool {
	raw, given := GivenForThisRun(EvaluatorEnabled)
	enabled, isSwitch := raw.(bool)
	return given && isSwitch && !enabled
}
