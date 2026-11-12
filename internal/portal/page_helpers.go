package portal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/assets"
)

// This file holds the view helpers of the server-rendered portal pages: the template function map,
// the navigation, and the themes. web/portal/input.css scans it for Tailwind class names, so every
// class written here must be a complete literal.

// themeInfo is one selectable daisyUI theme of the portal.
type themeInfo struct {
	Name  string
	Label string
	Dark  bool
}

// defaultTheme is the kit's own theme (web/portal/input.css), applied when no theme cookie is set.
const defaultTheme = "openspec"

// themeCookie remembers the theme chosen in this browser; the server renders it into <html data-theme>.
const themeCookie = "portal_theme"

// portalThemes holds the themes the menu offers: the kit's own and the ones of daisyUI that draw every
// page with text a person reads (4.5:1 against what is behind it). The browser tests draw every
// page in each of them; a theme that fails leaves this list and the one of web/portal/input.css,
// which TestTheMenuOffersTheThemesTheStyleSheetHolds holds to the same names.
var portalThemes = []themeInfo{
	{"openspec", "OpenSpec night shift", true},
	{"light", "Light", false}, {"night", "Night", true}, {"dracula", "Dracula", true},
	{"forest", "Forest", true}, {"cyberpunk", "Cyberpunk", false}, {"lofi", "Lo-fi", false},
	{"wireframe", "Wireframe", false}, {"acid", "Acid", false}, {"lemonade", "Lemonade", false},
	{"cmyk", "CMYK", false},
}

func validTheme(name string) bool {
	for _, t := range portalThemes {
		if t.Name == name {
			return true
		}
	}
	return false
}

// navItem is one sidebar link. Key is the path prefix that marks it active.
type navItem struct {
	Href  string
	Label string
	Icon  string
	Count int
	Tone  string // badge tone of Count: error, warning or success
}

type navGroup struct {
	Title string
	Items []navItem
}

// ---- value conversion ---------------------------------------------------------------------

func toList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case nil:
		return nil
	}
	var out []any
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	// What is not a list leaves out empty, which is the answer for it.
	_ = json.Unmarshal(b, &out)
	return out
}

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

func toStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == math.Trunc(x) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func parseWhen(v any) (time.Time, bool) {
	switch x := v.(type) {
	case time.Time:
		return x, !x.IsZero()
	case float64:
		if x <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(x), 0), true
	case int64:
		return time.Unix(x, 0), x > 0
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return time.Time{}, false
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "20060102T150405Z", "20060102-150405", "2006-01-02"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t, true
			}
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			return time.Unix(n, 0), true
		}
	}
	return time.Time{}, false
}

// ---- formatting ----------------------------------------------------------------------------

func spanText(sec int) string {
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	m := int(math.Round(float64(sec) / 60))
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	h := m / 60
	if h < 24 {
		if m%60 != 0 {
			return fmt.Sprintf("%dh %dm", h, m%60)
		}
		return fmt.Sprintf("%dh", h)
	}
	d := h / 24
	if h%24 != 0 {
		return fmt.Sprintf("%dd %dh", d, h%24)
	}
	return fmt.Sprintf("%dd", d)
}

func agoText(v any) string {
	t, ok := parseWhen(v)
	if !ok {
		return "–"
	}
	s := int(math.Round(time.Since(t).Seconds()))
	if s < 0 {
		return "in " + spanText(-s)
	}
	if s < 45 {
		return "just now"
	}
	return spanText(s) + " ago"
}

func bytesText(v any) string {
	n := toInt(v)
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1048576:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/1048576)
}

// calendarDaysBetween counts the calendar days from t to now in loc: 0 on the same day, 1 for
// yesterday. A day is not always 24 hours (23 or 25 across a DST change), so the hours between the two
// midnights are rounded, not truncated.
func calendarDaysBetween(t, now time.Time, loc *time.Location) int {
	y1, m1, d1 := t.In(loc).Date()
	y2, m2, d2 := now.In(loc).Date()
	return int(math.Round(time.Date(y2, m2, d2, 0, 0, 0, 0, loc).Sub(time.Date(y1, m1, d1, 0, 0, 0, 0, loc)).Hours() / 24))
}

// truncateRunes cuts s to at most max characters and marks the cut with "…", never inside a UTF-8
// sequence (a byte cut leaves invalid UTF-8, which renders as U+FFFD). max <= 0 keeps s whole.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i] + "…"
		}
		n++
	}
	return s
}

func dayLabel(v any) string {
	t, ok := parseWhen(v)
	if !ok {
		return "Undated"
	}
	t = t.Local()
	days := calendarDaysBetween(t, time.Now(), time.Local)
	switch days {
	case 0:
		return "Today"
	case 1:
		return "Yesterday"
	}
	if days > 300 {
		return t.Format("Monday, Jan 2, 2006")
	}
	return t.Format("Monday, Jan 2")
}

var (
	toneErrorRe   = regexp.MustCompile(`(fail|error|blocked|stale|stopped|violation|rejected|needs.human|preflight failed|unknown|regression|cannot|missing|dead)`)
	toneSuccessRe = regexp.MustCompile(`^(ok|done|verified|integrated|ready|active|reported|approved|pass|success|idle|closed|finished|enabled|alive|on path|selected)`)
	toneWarnRe    = regexp.MustCompile(`(run|flight|worktree|queued|proposed|pending|progress|planned|judged|paused|waiting|held|open|busy|repair)`)
)

// tone maps a status word to a daisyUI semantic color name ("" means neutral).
func tone(v any) string {
	s := strings.ToLower(toStr(v))
	switch {
	case toneErrorRe.MatchString(s):
		return "error"
	case toneSuccessRe.MatchString(s):
		return "success"
	case toneWarnRe.MatchString(s):
		return "warning"
	}
	return ""
}

// toneIcon is the icon that carries a tone's meaning next to its text, so status never relies on color.
func toneIcon(t string) string {
	switch t {
	case "error":
		return "octagon-alert"
	case "success":
		return "circle-check"
	case "warning":
		return "circle-dot"
	case "info":
		return "info"
	}
	return ""
}

// spriteURL is the address of the icon sprite, versioned by its content: a browser that cached an older
// sprite for a day (before it was served with no-cache) drew every icon added since as a blank, and a
// new query string is a new resource to it.
var spriteURL = func() string {
	b, err := fs.ReadFile(assets.FS, "portal_ui/icons.svg")
	if err != nil {
		return "/assets/icons.svg"
	}
	sum := sha256.Sum256(b)
	return "/assets/icons.svg?v=" + hex.EncodeToString(sum[:6])
}()

func iconHTML(name, class string) template.HTML {
	if class == "" {
		class = "size-4 shrink-0"
	}
	return template.HTML(`<svg class="` + template.HTMLEscapeString(class) + `" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><use href="` + spriteURL + `#` + template.HTMLEscapeString(name) + `"></use></svg>`)
}

// agentIconNames gives each agent the kit knows (cycle.SupportedAgents) a Lucide glyph. It is drawn only
// when the agent has no logo under portal_ui/agents/ (see agentLogo); an agent that is not listed, such as
// a custom binary added under commands: in .foreman-kit/agents.yaml, gets the generic bot.
var agentIconNames = map[string]string{
	"claude":       "sparkles",
	"codex":        "square-terminal",
	"copilot":      "git-pull-request",
	"hermes":       "feather",
	"opencode":     "code",
	"agy":          "orbit",
	"kilo":         "weight",
	"cline":        "brain-circuit",
	"pi":           "pi",
	"omp":          "sigma", // oh-my-pi, a fork of pi, publishes no mark of its own
	"crush":        "heart",
	"cursor-agent": "mouse-pointer-2",
	"zcode":        "braces",
}

// agentIconName returns the glyph of an agent by its name, "bot" when it has none.
func agentIconName(agent string) string {
	if name, ok := agentIconNames[strings.ToLower(strings.TrimSpace(agent))]; ok {
		return name
	}
	return "bot"
}

// agentLogos maps an agent to its logo file in portal_ui/agents/: the mark each vendor publishes on its
// own site (favicon or app icon), downloaded once and embedded. The marks are trademarks of their
// owners and only identify the agent they stand for.
var agentLogos = func() map[string]string {
	out := map[string]string{}
	entries, _ := fs.ReadDir(assets.FS, "portal_ui/agents")
	for _, e := range entries {
		name := e.Name()
		if agentLogoType(name) == "" {
			continue
		}
		out[strings.TrimSuffix(strings.TrimSuffix(name, ".svg"), ".png")] = name
	}
	return out
}()

// agentLogoType is the content type of a logo file name, "" when it is not one the portal serves.
func agentLogoType(name string) string {
	if strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		return ""
	}
	switch {
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	}
	return ""
}

// agentLogo returns the logo file of an agent, "" when it has none.
func agentLogo(agent string) string {
	return agentLogos[strings.ToLower(strings.TrimSpace(agent))]
}

// agentIcon draws an agent: its logo when the kit has one, otherwise its Lucide glyph.
func agentIcon(agent, class string) template.HTML {
	if f := agentLogo(agent); f != "" {
		if class == "" {
			class = "size-4"
		}
		return template.HTML(`<img src="/assets/agents/` + template.HTMLEscapeString(f) + `" alt="" aria-hidden="true" loading="lazy" decoding="async" class="` + template.HTMLEscapeString(class) + ` shrink-0 rounded-sm object-contain">`)
	}
	return iconHTML(agentIconName(agent), class)
}

// badgeClass returns the complete daisyUI badge classes of a tone.
func badgeClass(t string) string {
	switch t {
	case "error":
		return "badge badge-sm badge-soft badge-error"
	case "success":
		return "badge badge-sm badge-soft badge-success"
	case "warning":
		return "badge badge-sm badge-soft badge-warning"
	case "info":
		return "badge badge-sm badge-soft badge-info"
	case "primary":
		return "badge badge-sm badge-soft badge-primary"
	}
	return "badge badge-sm badge-ghost"
}

// badgeHTML renders a status badge: its text, plus an icon when the status has a tone.
func badgeHTML(text any, forced ...string) template.HTML {
	t := tone(text)
	if len(forced) > 0 {
		t = forced[0]
	}
	label := strings.ReplaceAll(toStr(text), "_", " ")
	var b strings.Builder
	b.WriteString(`<span class="` + badgeClass(t) + ` whitespace-nowrap">`)
	if ic := toneIcon(t); ic != "" {
		b.WriteString(string(iconHTML(ic, "size-3 shrink-0")))
	}
	b.WriteString(template.HTMLEscapeString(label))
	b.WriteString(`</span>`)
	return template.HTML(b.String())
}

// eventContext lists the fields of an event other than its time, name and change, as key=value pairs.
func eventContext(e any, max int) string {
	m, _ := e.(map[string]any)
	keys := make([]string, 0, len(m))
	for k := range m {
		switch k {
		case "ts", "event", "change", "id", "exec_id":
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		var s string
		switch v.(type) {
		case map[string]any, []any:
			b, _ := json.Marshal(v)
			s = string(b)
		default:
			s = toStr(v)
		}
		parts = append(parts, k+"="+s)
	}
	return truncateRunes(strings.Join(parts, " "), max)
}

// eventTone sorts an event by what it means for a person reading the stream: "error" when something
// failed or was refused (a blocked change, a failed gate or merge, a rejection by the evaluator, an agent that
// exited with an error or stalled), "warning" when it ran degraded or was stopped (a change held or
// deferred behind another one is waiting by design and stays neutral; an agent that went idle may
// yet work again), "success" when work landed, and "" for the routine rest.
func eventTone(e any) string {
	m, _ := e.(map[string]any)
	name := toStr(m["event"])
	switch name {
	case "change.blocked", "merge.base_broken", "merge.push_rejected", "merge.sent_back", "merge.invalid_manifest",
		"merge.work_loss_detected", "proposal.apply_failed", "backlog.unreadable", "backlog.invalid_name",
		"backlog.duplicate_name", "repo.pointer_rolled_back", "dispatcher.wait_timeout", "agent.stalled":
		return "error"
	case "change.retry", "change.rebuild", "change.stray_reverted", "evaluator.unavailable",
		"evaluator.route", "lease.stale_reclaimed", "agent.orphan_stopped", "system.paused", "agent.idle":
		return "warning"
	case "merge.integrated", "change.ready", "merge.base_recovered", "merge.base_accepted", "proposal.applied":
		return "success"
	case "agent.exit":
		// An agent a signal ended was stopped from outside (the kit, a person, the system): that is
		// not an error of the agent.
		switch code := toInt(m["code"]); {
		case code > 128:
			return "warning"
		case code != 0:
			return "error"
		}
	case "change.finished":
		// busy, deferred, held, paused and lost mean the change waits for another worker or change: neutral.
		switch toStr(m["result"]) {
		case "ready":
			return "success"
		case "stopped":
			return "warning"
		case "blocked":
			return "error"
		}
	case "evaluator.answer":
		switch toStr(m["status"]) {
		case "passed", "":
		case "abstained", "skipped":
			return "warning"
		default:
			return "error"
		}
	case "proposal.reviewed":
		if v, ok := m["approved"].(bool); ok && !v {
			return "error"
		}
	}
	return ""
}

// label turns a snake_case code into words.
func label(v any) string { return strings.ReplaceAll(toStr(v), "_", " ") }

// eventDescription says in one sentence what an event means, for the events people meet most; ""
// for the rest, which keep their name and context only.
func eventDescription(e any) string {
	m, _ := e.(map[string]any)
	repo, sha := toStr(m["repo"]), toStr(m["sha"])
	if len(sha) > 8 {
		sha = sha[:8]
	}
	switch toStr(m["event"]) {
	case "change.start":
		return "A worker started planning or building this change."
	case "change.ready":
		return "Built, and every gate passed: the change waits for the merger."
	case "change.finished":
		switch toStr(m["result"]) {
		case "ready":
			return "The worker finished: the change is built and ready to merge."
		case "blocked":
			return "The worker stopped: the change is blocked and needs a person."
		case "held":
			return "The worker waited: another change holds a lock this one needs. It runs again by itself."
		case "busy":
			return "Another worker already had this change; nothing was done twice."
		case "stopped":
			return "The worker was stopped before it finished (the daemon restarted or someone stopped it)."
		}
	case "change.held":
		return "Not started yet: it conflicts with a change in flight (" + toStr(m["conflicts"]) + "). It runs again by itself."
	case "change.blocked":
		return "Quarantined: it needs a person before it runs again (" + label(m["reason"]) + ")."
	case "change.retry":
		return "Tried again after an earlier failure."
	case "agent.exit":
		switch code := toInt(m["code"]); {
		case code > 128:
			return "The agent was stopped before it finished, by the signal " + signalName(code-128) + " (exit code " + toStr(m["code"]) + ")."
		case code != 0:
			return "The agent exited with an error (code " + toStr(m["code"]) + ")."
		}
		return "The agent finished its prompt."
	case "agent.idle":
		return "The agent has shown no activity for " + spanText(toInt(m["idle_seconds"])) + ": no file changed, nothing printed and no command of its own used CPU. If that lasts " +
			spanText(toInt(m["limit_seconds"])) + ", the idle watchdog stops it."
	case "agent.resumed":
		return "The agent works again, after " + spanText(toInt(m["idle_seconds"])) + " without activity."
	case "agent.stalled":
		return "The idle watchdog stopped the agent after " + spanText(toInt(m["idle_seconds"])) + " without activity. It is not tried again in that call: the next agent of the role, or a later dispatch, continues."
	case "evaluator.answer":
		switch toStr(m["status"]) {
		case "passed":
			return "The evaluator, a calibrated judge, approved the " + label(m["gate"]) + " check."
		case "abstained":
			return "The evaluator could not tell for the " + label(m["gate"]) + " check (" + toStr(m["mode"]) + " mode)."
		default:
			return "The evaluator rejected the " + label(m["gate"]) + " check."
		}
	case "evaluator.unavailable":
		return "The evaluator could not be asked: " + toStr(m["detail"]) + "."
	case "merge.integrated":
		return "Merged: its commits are on the base branches and its specs are archived."
	case "merge.sent_back":
		return "The merger sent it back to be rebuilt (" + label(m["reason"]) + ")."
	case "merge.base_broken":
		return "The base branch of " + repo + " fails its own checks, before any change is applied: the changes that touch it wait for a person."
	case "merge.base_rechecked":
		return "The merger checked the base of " + repo + " (" + sha + ") once more on its own, in case the failure had healed."
	case "merge.base_recovered":
		return "The base of " + repo + " passes its checks again: the changes it held can merge."
	case "merge.base_accepted":
		return "A person accepted the base of " + repo + " (" + sha + ") with its failures: the held changes merge with their own tests."
	case "merge.push_rejected":
		return "The push to " + repo + " was rejected; the merger tries again on its next pass."
	case "repo.pointer_rolled_back":
		return "The umbrella's pointer to " + repo + " went back to an older commit, dropping " + toStr(m["lost_commits"]) + " commit(s)."
	case "main.fast_forwarded":
		return "The base branch moved forward to include new commits."
	case "lease.stale_reclaimed":
		return "A worker had died holding this change; its lease was freed."
	case "dispatcher.start":
		return "The dispatcher started looking for work."
	case "dispatcher.finish":
		return "The dispatcher finished its pass."
	}
	return ""
}

// signalName names the signals that end an agent, and gives the number of any other.
func signalName(n int) string {
	switch n {
	case 1:
		return "HUP"
	case 2:
		return "INT"
	case 9:
		return "KILL"
	case 15:
		return "TERM"
	}
	return strconv.Itoa(n)
}

// eventField renders one field of an event: nested values as JSON, the rest as text.
func eventField(e any, key string) string {
	m, _ := e.(map[string]any)
	switch v := m[key].(type) {
	case map[string]any, []any:
		b, _ := json.MarshalIndent(v, "", "  ")
		return string(b)
	default:
		return toStr(v)
	}
}

func dict(kv ...any) map[string]any {
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[toStr(kv[i])] = kv[i+1]
	}
	return m
}

func encPath(p any) string {
	parts := strings.Split(toStr(p), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// pagerLink is one entry of a pager: a page number, or a gap when Gap is set.
type pagerLink struct {
	N       int
	Href    string
	Current bool
	Gap     bool
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"icon":      func(name string) template.HTML { return iconHTML(name, "") },
		"iconc":     func(name, class string) template.HTML { return iconHTML(name, class) },
		"agentIcon": agentIcon,
		"spriteURL": func() string { return spriteURL },
		// The version of the running binary and the commit it was built from.
		"kitVersion": func() string { return assets.Version },
		"kitCommit":  func() string { return assets.SourceCommit },
		"agentLogo":  agentLogo,
		"when": func(v any) template.HTML {
			t, ok := parseWhen(v)
			if !ok {
				return `<span class="opacity-50">–</span>`
			}
			return template.HTML(`<time class="whitespace-nowrap" datetime="` + t.UTC().Format(time.RFC3339) + `" title="` + t.Local().Format("Mon Jan 2 2006, 15:04:05") + `">` + agoText(t) + `</time>`)
		},
		"clock": func(v any) string {
			if t, ok := parseWhen(v); ok {
				return t.Local().Format("15:04")
			}
			return ""
		},
		"stamp": func(v any) string {
			if t, ok := parseWhen(v); ok {
				return t.Local().Format("Jan 2 2006, 15:04:05")
			}
			return toStr(v)
		},
		"day":     dayLabel,
		"bytes":   bytesText,
		"label":   label,
		"upper":   func(v any) string { return strings.ToUpper(toStr(v)) },
		"badge":   badgeHTML,
		"num":     toInt,
		"str":     toStr,
		"add":     func(a, b any) int { return toInt(a) + toInt(b) },
		"list":    func(xs ...any) []any { return xs },
		"dict":    dict,
		"evctx":   eventContext,
		"evtone":  eventTone,
		"evdesc":  eventDescription,
		"enc":     func(v any) string { return url.PathEscape(toStr(v)) },
		"qenc":    func(v any) string { return url.QueryEscape(toStr(v)) },
		"encpath": encPath,
		"join": func(v any, sep string) string {
			if s, ok := v.(string); ok {
				return s
			}
			var out []string
			for _, x := range toList(v) {
				out = append(out, toStr(x))
			}
			return strings.Join(out, sep)
		},
	}
}
