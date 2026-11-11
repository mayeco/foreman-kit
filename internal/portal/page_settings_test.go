package portal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/settings/settingstest"
)

// dropKey takes the line of key out of its file in the application at root: the key is then missing.
func dropKey(t *testing.T, root string, key settings.Key) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(key.FilePath()))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parts := key.Parts()
	leaf := parts[len(parts)-1] + ":"
	var kept []string
	dropped := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), leaf) {
			dropped++
			continue
		}
		kept = append(kept, line)
	}
	if dropped != 1 {
		t.Fatalf("%s writes %s on %d lines, want 1", key.FilePath(), leaf, dropped)
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

// firstOfKind returns the first setting of the catalog of a kind.
func firstOfKind(t *testing.T, kind settings.Kind) settings.Setting {
	t.Helper()
	for _, s := range settings.All() {
		if s.Kind == kind {
			return s
		}
	}
	t.Fatalf("the catalog has no setting of kind %s", kind)
	return settings.Setting{}
}

// TestSettingsListEveryFileAndKeyOfTheCatalog: System → Settings and /api/system/settings list the
// configuration files and the settings of each as the catalog of the kit has them, in its order,
// with what each file holds. Nothing is named by the portal: a key the catalog gains is listed.
func TestSettingsListEveryFileAndKeyOfTheCatalog(t *testing.T) {
	srv, _, _ := setupTestPortal(t)

	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/system/settings", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/system/settings = %d %s", code, raw)
	}
	catalog := settings.Files()
	files, _ := data["files"].([]any)
	if len(files) != len(catalog) {
		t.Fatalf("the settings list %d files, and the catalog has %d: %s", len(files), len(catalog), raw)
	}
	for i, f := range catalog {
		got, _ := files[i].(map[string]any)
		if got["name"] != f.Name || got["path"] != f.Path || got["about"] != f.About || got["read_by"] != f.ReadBy {
			t.Errorf("the file %d of the settings is %v, and the catalog has %+v", i, got, f)
		}
		if errs, _ := got["errors"].([]any); len(errs) != 0 {
			t.Errorf("the check finds errors in %s as the kit installs it: %v", f.Path, errs)
		}
	}
	// With nothing asked for and nothing wrong, the first file is the one shown; no process waits.
	if shown, _ := data["file"].(map[string]any); shown["name"] != catalog[0].Name {
		t.Errorf("the file shown is %v, want %s", shown["name"], catalog[0].Name)
	}
	if waits, _ := data["waits"].([]any); len(waits) != 0 {
		t.Errorf("a process waits with the configuration the kit installs: %v", waits)
	}
	if _, now, raw, _ := doReq(t, srv, http.MethodGet, "/api/dashboard", nil); now["config_waits"] == nil || len(now["config_waits"].([]any)) != 0 {
		t.Errorf("Dashboard says a process waits for the configuration the kit installs: %s", raw)
	}
	if others, _ := data["others"].([]any); len(others) == 0 {
		t.Errorf("the settings do not say what else the check looks at: %s", raw)
	}

	for _, f := range catalog {
		address := "/system/settings?file=" + url.QueryEscape(f.Name)
		code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api"+address, nil)
		if code != http.StatusOK {
			t.Fatalf("GET /api%s = %d %s", address, code, raw)
		}
		_, body := overviewPage(t, srv, address)
		wantOnPage(t, "the settings of "+f.Path, body, f.Path, f.About, f.ReadBy)
		text := textOf(body)
		want := settings.In(f.Name)
		rows, _ := data["settings"].([]any)
		if len(rows) != len(want) {
			t.Fatalf("%s lists %d settings, and the catalog has %d of that file", address, len(rows), len(want))
		}
		for i, s := range want {
			row, _ := rows[i].(map[string]any)
			if row["key"] != string(s.Key) || row["option"] != s.Key.Path() || row["file"] != s.Key.FilePath() || row["kind"] != string(s.Kind) {
				t.Errorf("the setting %d of %s is %v, and the catalog has %s", i, f.Path, row, s.Key)
				continue
			}
			if asStr(row["means"]) == "" || asStr(row["accepts"]) == "" || asStr(row["read_by"]) == "" {
				t.Errorf("%s does not say what it decides, what it accepts or who reads it: %v", s.Key, row)
			}
			if row["set"] != true || (asStr(row["written"]) == "" && asStr(row["lines"]) == "") {
				t.Errorf("%s is not written in the file the kit installs: %v", s.Key, row)
			}
			if row["settable"] != (s.Scalar() || s.Kind == settings.KindAgents) {
				t.Errorf("%s, of kind %s, says settable = %v", s.Key, s.Kind, row["settable"])
			}
			if !strings.Contains(text, s.Key.Path()) || !strings.Contains(text, s.Means) {
				t.Errorf("the page of %s does not list %s with what it decides", f.Path, s.Key.Path())
			}
			if !strings.Contains(body, `href="`+settingHref(s.Key)+`"`) {
				t.Errorf("the page of %s does not lead to the editor of %s", f.Path, s.Key.Path())
			}
		}
	}

	// A file and a setting the catalog does not have are refused, and so is a setting asked for in
	// the file of another.
	other := catalog[1].Name
	for _, address := range []string{
		"/system/settings?file=no-such-file",
		"/system/settings?edit=pipeline.no.such.key",
		"/system/settings?file=" + other + "&edit=" + string(settings.In(catalog[0].Name)[0].Key),
	} {
		code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api"+address, nil)
		if c, _ := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" {
			t.Errorf("GET /api%s = %d %s", address, code, raw)
		}
		if code, _, _ := getPage(t, srv, address); code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", address, code)
		}
	}
}

// TestSettingsSayWhatKeepsAFileFromBeingUsed: a key that is missing is said where a person looks for
// it: the file with the error is the one shown, the check names the key, the setting says it is
// missing and holds nothing in its place, and the processes that read the file are listed as the ones
// that start nothing new. A value --set gave the run that started the portal is said on the row of
// its key.
func TestSettingsSayWhatKeepsAFileFromBeingUsed(t *testing.T) {
	srv, _, root := setupTestPortal(t)
	key := settings.EvaluatorTimeout
	if _, ok := settings.Lookup(key); !ok {
		t.Fatalf("%s is not a setting", key)
	}
	dropKey(t, root, key)

	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/system/settings", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/system/settings = %d %s", code, raw)
	}
	shown, _ := data["file"].(map[string]any)
	if shown["name"] != key.File() {
		t.Fatalf("the file shown is %v, want the one with the error, %s", shown["name"], key.File())
	}
	errs, _ := shown["errors"].([]any)
	if len(errs) != 1 || !strings.Contains(asStr(errs[0]), key.Path()) || !strings.Contains(asStr(errs[0]), "missing") {
		t.Errorf("the check of %s says %v, want that %s is missing", key.FilePath(), errs, key.Path())
	}
	if n, _ := data["errors"].(float64); n < 1 {
		t.Errorf("the settings count %v errors", data["errors"])
	}
	var row map[string]any
	rows, _ := data["settings"].([]any)
	for _, it := range rows {
		if r, _ := it.(map[string]any); r["key"] == string(key) {
			row = r
		}
	}
	if row == nil || row["set"] != false || row["problem"] != "missing" || asStr(row["written"]) != "" || asStr(row["in_effect"]) != "" {
		t.Errorf("the setting that is missing reads %v", row)
	}
	// The file of the evaluator is read by the dispatcher, the merger and the cycle: each waits.
	waiting := map[string]string{}
	waits, _ := data["waits"].([]any)
	for _, it := range waits {
		w, _ := it.(map[string]any)
		waiting[asStr(w["process"])] = asStr(w["why"])
	}
	for _, process := range []string{"The dispatcher", "The merger", "The cycle"} {
		if why := waiting[process]; !strings.Contains(why, key.Path()) || !strings.Contains(why, key.FilePath()) {
			t.Errorf("%s waits for %q, want the key that is missing and its file", process, why)
		}
	}
	if why, waits := waiting["The daemon, when it starts"]; waits {
		t.Errorf("the daemon waits for a file it does not read: %s", why)
	}

	// Dashboard says it first: it stops the kit the way a pause does.
	_, now, raw, _ := doReq(t, srv, http.MethodGet, "/api/dashboard", nil)
	if stopped, _ := now["config_waits"].([]any); len(stopped) != 3 {
		t.Errorf("Dashboard lists %d processes that wait for the configuration, want 3: %s", len(stopped), raw)
	}
	_, front := overviewPage(t, srv, "/")
	wantOnPage(t, "Dashboard with a key missing", front,
		"3 processes start nothing new until the configuration is mended", "The dispatcher:", key.Path(), `href="/system/settings"`)

	_, body := overviewPage(t, srv, "/system/settings")
	wantOnPage(t, "the settings with a key missing", body,
		"3 processes start nothing new until the configuration is mended", "The dispatcher:", "The merger:", "The cycle:",
		key.FilePath(), key.Path()+": missing", "missing", "1 error")
	if !strings.Contains(body, `badge-error tabular-nums">1<span class="sr-only"> error</span>`) || !strings.Contains(body, `href="/system/settings?file=`+key.File()+`">`+key.FilePath()+`</a>`) {
		t.Errorf("the tab of %s does not count its error:\n%s", key.File(), body)
	}

	// What --set gave the run that started this portal replaces what the file holds: the row of
	// the key says so, with the value the commands run from here read.
	settingstest.Give(t, settings.Workers, "7")
	given := "--set " + string(settings.Workers) + "=7"
	address := "/system/settings?file=" + url.QueryEscape(settings.Workers.File())
	_, data, raw, _ = doReq(t, srv, http.MethodGet, "/api"+address, nil)
	rows, _ = data["settings"].([]any)
	row = nil
	for _, it := range rows {
		if r, _ := it.(map[string]any); r["key"] == string(settings.Workers) {
			row = r
		}
	}
	if row == nil || row["overridden_by"] != given || row["in_effect"] != "7" || row["written"] != "2" {
		t.Errorf("the setting --set replaces reads %v", row)
	}
	_, body = overviewPage(t, srv, address)
	wantOnPage(t, "the settings with a key overridden", body, given, "replaces it for what the process of this portal runs, until it stops")
	_, body = overviewPage(t, srv, "/system/settings?edit="+url.QueryEscape(string(settings.Workers)))
	wantOnPage(t, "the editor of a key that is overridden", body, given, "was given to the process of this portal when it started", "it reads 7")
}

// TestASettingIsChangedFromItsEditor: the editor of a setting says what the catalog says of it and
// offers the control its kind asks for; POST /api/system/settings checks the value as `foreman-kit
// config set` does, rewrites only the line of the key, and refuses what the kit does not do.
func TestASettingIsChangedFromItsEditor(t *testing.T) {
	srv, _, root := setupTestPortal(t)
	workers := settings.Of(settings.Workers)

	// A number: a field that holds the value of now.
	_, body := overviewPage(t, srv, "/system/settings?edit="+url.QueryEscape(string(workers.Key)))
	wantOnPage(t, "the editor of a number", body, "Change", workers.Key.Path(), workers.Key.FilePath(), workers.Means, workers.ReadBy, workers.Expected(), "Write it to the file", "Only check it")
	for _, want := range []string{`id="edit"`, `data-api="/api/system/settings"`, `name="key" value="` + string(workers.Key) + `"`, `name="value" value="2"`, `name="dry_run" value="1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the editor of %s lacks %s", workers.Key, want)
		}
	}
	// A word among several: a list of them, the one of now chosen.
	enum := settings.Of(settings.OutsideCommits)
	_, body = overviewPage(t, srv, "/system/settings?edit="+url.QueryEscape(string(enum.Key)))
	for _, v := range enum.Values {
		want := `<option value="` + v + `"`
		if v == enum.Default {
			want += ` selected`
		}
		if !strings.Contains(body, want+">") {
			t.Errorf("the editor of %s does not offer %s", enum.Key, want)
		}
	}
	// A switch: true and false.
	onOff := firstOfKind(t, settings.KindBool)
	_, body = overviewPage(t, srv, "/system/settings?edit="+url.QueryEscape(string(onOff.Key)))
	if !strings.Contains(body, `<option value="true"`) || !strings.Contains(body, `<option value="false"`) {
		t.Errorf("the editor of %s does not offer true and false", onOff.Key)
	}
	// What holds more than one value is not changed from the portal: the editor says how it is.
	commands := firstOfKind(t, settings.KindCommands)
	_, body = overviewPage(t, srv, "/system/settings?edit="+url.QueryEscape(string(commands.Key)))
	wantOnPage(t, "the editor of the commands", body, "The portal does not change this one", commands.Change, "Show what the file holds")
	if strings.Contains(body, `data-api="/api/system/settings"`) {
		t.Errorf("the editor of %s offers a form the action would refuse", commands.Key)
	}

	path := filepath.Join(root, filepath.FromSlash(workers.Key.FilePath()))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	commentsOf := func(raw []byte) (n int) {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				n++
			}
		}
		return n
	}
	set := func(in map[string]any) (int, map[string]any, string) {
		code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/system/settings", in)
		return code, data, raw
	}
	unchanged := func(when string) {
		t.Helper()
		if now, _ := os.ReadFile(path); !bytes.Equal(now, before) {
			t.Fatalf("%s changed %s:\n%s", when, workers.Key.FilePath(), now)
		}
	}

	// A value the key does not accept is refused, checked only or not; a check writes nothing.
	for _, dry := range []bool{true, false} {
		code, data, raw := set(map[string]any{"key": string(workers.Key), "value": "banana", "dry_run": dry})
		if c, m := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" || !strings.Contains(m, "banana") {
			t.Errorf("setting %s to banana (dry_run %v) = %d %s", workers.Key, dry, code, raw)
		}
		unchanged("a value the key does not accept")
	}
	code, data, raw := set(map[string]any{"key": string(workers.Key), "value": "3", "dry_run": true})
	if code != http.StatusOK || !strings.Contains(asStr(data["output"]), "would change") || !strings.Contains(asStr(data["output"]), "2 -> 3") {
		t.Errorf("checking %s = 3 answered %d %s", workers.Key, code, raw)
	}
	unchanged("a check")

	// The change rewrites the line of the key, with the number unquoted, and keeps every comment.
	code, data, raw = set(map[string]any{"key": string(workers.Key), "value": "3"})
	if code != http.StatusOK || !strings.Contains(asStr(data["output"]), "changed "+workers.Key.FilePath()) || !strings.Contains(asStr(data["output"]), "2 -> 3") {
		t.Fatalf("setting %s = 3 answered %d %s", workers.Key, code, raw)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "workers: 3") || strings.Contains(string(after), `"3"`) {
		t.Errorf("the file does not hold an unquoted 3:\n%s", after)
	}
	if commentsOf(after) != commentsOf(before) {
		t.Errorf("the change kept %d of the %d comment lines of the file", commentsOf(after), commentsOf(before))
	}
	_, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/system/settings?edit="+url.QueryEscape(string(workers.Key)), nil)
	if editing, _ := data["editing"].(map[string]any); editing["written"] != "3" || editing["as_shipped"] != false || editing["shipped"] != "2" {
		t.Errorf("after the change the setting reads %v: %s", data["editing"], raw)
	}
	// Asked again, it says the file already holds the value.
	if code, data, raw = set(map[string]any{"key": string(workers.Key), "value": "3"}); code != http.StatusOK || !strings.Contains(asStr(data["output"]), "already") {
		t.Errorf("setting %s to the value it holds answered %d %s", workers.Key, code, raw)
	}
	// The agents of a role are changed too: they are one line of their file.
	agents := settings.RoleAgents("plan")
	if code, data, raw = set(map[string]any{"key": string(agents), "value": "hermes, claude"}); code != http.StatusOK || !strings.Contains(asStr(data["output"]), "changed") {
		t.Errorf("setting %s answered %d %s", agents, code, raw)
	}

	// What the action refuses: no key, a key that is not a setting, a setting of more than one
	// value, a field it does not take.
	for name, in := range map[string]map[string]any{
		"no key":                           {"value": "3"},
		"a key that is not a setting":      {"key": "pipeline.no.such.key", "value": "3"},
		"a setting of more than one value": {"key": string(commands.Key), "value": "x"},
		"a field the action does not take": {"key": string(workers.Key), "value": "4", "file": "pipeline"},
	} {
		code, data, raw := set(in)
		if c, _ := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" {
			t.Errorf("%s: POST /api/system/settings = %d %s", name, code, raw)
		}
	}

	// A file that is not YAML is left byte for byte as it was, not replaced by one that holds only
	// the key.
	cyclePath := filepath.Join(root, filepath.FromSlash(settings.ProposalsApprovedBy.FilePath()))
	broken := []byte("proposals:\n  approved_by: evaluator\n\tmemory: [unclosed\n")
	if err := os.WriteFile(cyclePath, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	code, data, raw = set(map[string]any{"key": string(settings.ProposalsApprovedBy), "value": "human"})
	if c, _ := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" {
		t.Errorf("setting a key of a file that is not YAML = %d %s", code, raw)
	}
	if now, _ := os.ReadFile(cyclePath); !bytes.Equal(now, broken) {
		t.Errorf("the file that is not YAML was rewritten:\n%s", now)
	}
	// The page still answers, and says what is wrong with the file.
	_, body = overviewPage(t, srv, "/system/settings")
	wantOnPage(t, "the settings with a file that is not YAML", body, settings.ProposalsApprovedBy.FilePath(), "The cycle:")
}

// TestTheActionChangesWhatThePageSaysItChanges: the page says of every setting whether the portal
// changes it, and its editor holds the value of now the way a person types it; the action agrees
// with the page. Asked only to check that value, it takes it for the settings the editor has a form
// for, and refuses the others. The rule is the catalog's (settings.Setting.Settable), which
// `foreman-kit config set` follows.
func TestTheActionChangesWhatThePageSaysItChanges(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	forms, refused := 0, 0
	for _, s := range settings.All() {
		rep, err := srv.work.Settings("", string(s.Key))
		if err != nil || rep.Editing == nil {
			t.Fatalf("the editor of %s: %v", s.Key, err)
		}
		if rep.Editing.Settable != config.Settable(s) {
			t.Errorf("%s (%s): the page says the portal changes it: %v, and the catalog says %v", s.Key, s.Kind, rep.Editing.Settable, config.Settable(s))
		}
		code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/system/settings", map[string]any{"key": string(s.Key), "value": rep.Editing.Typed, "dry_run": true})
		switch c, _ := errorOf(data); {
		case rep.Editing.Settable && code == http.StatusOK:
			forms++
		case !rep.Editing.Settable && code == http.StatusBadRequest && c == "invalid_input":
			refused++
		default:
			t.Errorf("%s (%s, the portal changes it: %v): checking the value its editor holds, %q, answered %d %s", s.Key, s.Kind, rep.Editing.Settable, rep.Editing.Typed, code, raw)
		}
	}
	if forms == 0 || refused == 0 {
		t.Errorf("the action took %d settings and refused %d: the catalog has both kinds", forms, refused)
	}
}

// recommending makes the kit of a test portal answer as one with enough on record: a recommendation
// for the idle limit, what it saw for two settings, and records that go back a week.
func recommending(srv *Server, rows int) {
	kitOf(srv).recommend = func(_ context.Context, _, window string) (cycle.Recommendations, error) {
		windows := []cycle.RecommendWindow{{Name: "24h", Available: true}, {Name: "7d", Available: true}, {Name: "15d"}, {Name: "30d"}}
		if window == "" {
			window = windows[0].Name
		}
		known := false
		for _, w := range windows {
			known = known || w.Name == window
		}
		if !known {
			return cycle.Recommendations{}, fmt.Errorf("no window %q: it is one of 24h, 7d, 15d, 30d", window)
		}
		now := time.Now().UTC()
		rec := cycle.Recommendations{
			Window: window, From: now.Add(-24 * time.Hour), To: now, HistoryHours: 40, MinimumHours: 24, Ready: true, Windows: windows,
			Rows: []cycle.Recommendation{},
			Measures: []cycle.SettingMeasure{
				{Key: string(settings.IdleTimeout), Option: settings.IdleTimeout.Path(), Seen: "10 agents were stopped for showing no work"},
				{Key: string(settings.Workers), Option: settings.Workers.Path(), Seen: "every worker was busy 12% of the time"},
			},
		}
		if rows > 0 {
			rec.Rows = append(rec.Rows, cycle.Recommendation{
				Key: string(settings.IdleTimeout), Option: settings.IdleTimeout.Path(), File: settings.IdleTimeout.FilePath(),
				Current: "20m", Recommended: "10m",
				Why:         "9 of the 10 agents stopped for showing no work had done nothing for 10 minutes",
				Improvement: "1h 30m of workers waiting on an agent that had stopped",
				Command:     "foreman-kit config set " + string(settings.IdleTimeout) + " 10m",
			})
		}
		return rec, nil
	}
}

// TestRecommendationsShowWhatTheKitWouldChange: the tab of the recommendations exists once the kit
// has enough on record. It shows the evaluation of the kit as it is: each setting with its value of
// now, the one recommended, why and what it would have gained, and the way to the editor of that
// setting. The portal applies nothing.
func TestRecommendationsShowWhatTheKitWouldChange(t *testing.T) {
	srv, _, _ := setupTestPortal(t)

	// A kit with no history: Settings has no tab for them, and the page says how far it is.
	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/system/settings", nil)
	if code != http.StatusOK || data["recommends"] != false {
		t.Fatalf("GET /api/system/settings of a kit with no history = %d %s", code, raw)
	}
	_, body := overviewPage(t, srv, "/system/settings")
	if strings.Contains(body, `href="/system/recommendations"`) {
		t.Error("the settings of a kit with no history lead to recommendations it does not have")
	}
	_, body = overviewPage(t, srv, "/system/recommendations")
	wantOnPage(t, "the recommendations of a kit with no history", body, "Nothing yet.", "A dispatcher has run for 0.0 of the 24 hours the kit asks for")
	if code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/system/recommendations", nil); code != http.StatusOK || data["ready"] != false {
		t.Errorf("GET /api/system/recommendations of a kit with no history = %d %s", code, raw)
	}

	// With enough on record, the tab is there.
	recommending(srv, 1)
	if _, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/system/settings", nil); data["recommends"] != true {
		t.Errorf("the settings of a kit with history do not say it recommends: %s", raw)
	}
	_, body = overviewPage(t, srv, "/system/settings")
	if !strings.Contains(body, `href="/system/recommendations"`) {
		t.Error("the settings of a kit with history do not lead to its recommendations")
	}

	_, body = overviewPage(t, srv, "/system/recommendations")
	wantOnPage(t, "the recommendations", body,
		"Recommendations", settings.IdleTimeout.Path(), settings.IdleTimeout.FilePath(), "20m", "10m",
		"9 of the 10 agents stopped for showing no work had done nothing for 10 minutes",
		"1h 30m of workers waiting on an agent that had stopped",
		"24 hours", "7 days", "What the kit saw for each setting it evaluates", "every worker was busy 12% of the time")
	if !strings.Contains(body, `href="`+settingHref(settings.IdleTimeout)+`"`) || !strings.Contains(body, `href="`+settingHref(settings.Workers)+`"`) {
		t.Error("the recommendations do not lead to the editor of each setting")
	}
	// A stretch the records do not reach is not offered, and nothing here applies a value.
	text := textOf(body)
	for _, absent := range []string{"15 days", "30 days", "do not go back that far"} {
		if strings.Contains(text, absent) {
			t.Errorf("the recommendations say %q", absent)
		}
	}
	if strings.Contains(body, "data-post=") || strings.Contains(body, `data-api="/api/system/settings"`) {
		t.Error("the recommendations offer to apply a value")
	}
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/system/recommendations?window=7d", nil)
	rows, _ := data["rows"].([]any)
	if code != http.StatusOK || data["ready"] != true || data["window"] != "7d" || len(rows) != 1 {
		t.Fatalf("GET /api/system/recommendations?window=7d = %d %s", code, raw)
	}
	if row, _ := rows[0].(map[string]any); row["key"] != string(settings.IdleTimeout) || row["current"] != "20m" || row["recommended"] != "10m" {
		t.Errorf("the recommendation reads %v", rows[0])
	}

	// Asked for by its address, a stretch the records do not reach is shown, and says so.
	_, body = overviewPage(t, srv, "/system/recommendations?window=15d")
	wantOnPage(t, "a stretch the records do not reach", body, "15 days", "The records do not go back that far")
	// A stretch the kit does not evaluate is refused.
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/system/recommendations?window=2y", nil)
	if c, m := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" || !strings.Contains(m, "2y") {
		t.Errorf("GET /api/system/recommendations?window=2y = %d %s", code, raw)
	}

	// Enough on record and nothing to change is said as that.
	recommending(srv, 0)
	_, body = overviewPage(t, srv, "/system/recommendations")
	wantOnPage(t, "the recommendations with nothing to change", body, "Nothing to change.", "What the kit saw for each setting it evaluates")

	// A kit that cannot read its records is a failure of the portal, not of what was asked.
	kitOf(srv).recommend = func(context.Context, string, string) (cycle.Recommendations, error) {
		return cycle.Recommendations{}, errors.New("the database is locked")
	}
	if code, _, raw, _ = doReq(t, srv, http.MethodGet, "/api/system/recommendations?window=7d", nil); code != http.StatusInternalServerError {
		t.Errorf("GET /api/system/recommendations of a kit that cannot read its records = %d %s", code, raw)
	}
}
