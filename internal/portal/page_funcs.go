package portal

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

// What the templates of the redesigned pages call. These take the values of the store as they are
// (a status, an RFC 3339 instant, a number of seconds) and say them the one way the portal says
// them. web/portal/input.css scans this file for class names: every class here is a whole literal.

func pageFuncs() template.FuncMap {
	return template.FuncMap{
		"state":     stateBadge,
		"word":      wordingOf,
		"at":        atHTML,
		"took":      tookText,
		"seconds":   secondsText,
		"millis":    millisText,
		"firstLine": firstLine,
		"execHref":  executionHref,
		"execName":  executionName,
		"execRow": func(d store.ExecutionDetail) executionRow {
			return executionRow{ExecutionDetail: d, Name: executionName(d.Execution), About: executionAbout(d.Execution)}
		},
		"readAt":        readAtHTML,
		"idleLimitName": func() string { return settings.IdleTimeout.Path() },
		"idleDelayName": func() string { return settings.CycleEveryWhenOutOfWork.Where() },
		"idleDelayKey":  func() string { return string(settings.CycleEveryWhenOutOfWork) },
		"settingHref":   settingHref,
		"baseCheck":     store.BaseCheckLabel,
		"riskBlock":     func(reason any) bool { r, _ := reason.(string); return store.RiskBlock(r) },
		"count":         countText,
		"times":         func(n int) string { return countText(n, "time", "times") },
		"systemPages":   func() []navItem { return systemPages },
		"cycleTone":     cycleTone,
		"usd":           func(v float64) string { return fmt.Sprintf("$%.4f", v) },
		"share":         shareText,
		"codeSpans":     codeSpans,
	}
}

// stateBadge draws a recorded value as a badge: its words, the color of its tone and, with a tone,
// the icon that says the same without color. What the value means is its tooltip.
func stateBadge(vocabulary, value string) template.HTML {
	w := wordingOf(vocabulary, value)
	var b strings.Builder
	b.WriteString(`<span class="` + badgeClass(w.Tone) + ` whitespace-nowrap"`)
	if w.Means != "" {
		b.WriteString(` title="` + template.HTMLEscapeString(w.Means) + `"`)
	}
	b.WriteString(`>`)
	if icon := toneIcon(w.Tone); icon != "" {
		b.WriteString(string(iconHTML(icon, "size-3 shrink-0")))
	}
	b.WriteString(template.HTMLEscapeString(w.Text))
	b.WriteString(`</span>`)
	return template.HTML(b.String())
}

// atHTML draws an instant as a <time>: how long ago for one of today, its date and hour for an
// earlier one, and always the exact time with its zone as the tooltip and the instant itself in
// datetime.
func atHTML(v any) template.HTML {
	t, ok := parseWhen(v)
	if !ok {
		return `<span class="opacity-50">–</span>`
	}
	return template.HTML(`<time class="whitespace-nowrap" datetime="` + t.UTC().Format(time.RFC3339) +
		`" title="` + template.HTMLEscapeString(t.Local().Format("Mon Jan 2 2006, 15:04:05 MST")) + `">` +
		template.HTMLEscapeString(atText(t, time.Now())) + `</time>`)
}

// readAtHTML draws the instant a page was read, to the second: what the page shows was true then.
func readAtHTML(t time.Time) template.HTML {
	return template.HTML(`<time datetime="` + t.UTC().Format(time.RFC3339) + `" title="` +
		template.HTMLEscapeString(t.Local().Format("Mon Jan 2 2006, 15:04:05 MST")) + `">` + t.Local().Format("15:04:05") + `</time>`)
}

func atText(t, now time.Time) string {
	local := t.Local()
	switch {
	case calendarDaysBetween(local, now, time.Local) == 0:
		return agoText(t)
	case local.Year() == now.Local().Year():
		return local.Format("Jan 2, 15:04")
	}
	return local.Format("Jan 2 2006, 15:04")
}

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }

// secondsText says a number of whole seconds. None is less than one second, not a number of
// milliseconds: the store counts in seconds.
func secondsText(n int) string {
	if n < 1 {
		return "under 1s"
	}
	return durationText(seconds(n))
}

// durationText says a duration the way a person reads it: 850ms, 42s, 3m 12s, 2h 05m, 1d 4h.
func durationText(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	s := int(d.Round(time.Second) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	case s < 86400:
		return fmt.Sprintf("%dh %02dm", s/3600, s%3600/60)
	}
	return fmt.Sprintf("%dd %dh", s/86400, s%86400/3600)
}

// millisText says a time that was measured in milliseconds, like the run of a gate. Under ten
// seconds it keeps a tenth of a second: gates that take 1.2s and 1.8s are not both "2s".
func millisText(n int64) string {
	d := time.Duration(n) * time.Millisecond
	if d < time.Second || d.Round(100*time.Millisecond) >= 10*time.Second {
		return durationText(d)
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// codeSpans draws a line of a Markdown file with its `code` spans as code and the rest as it is
// written: the plan of a change names files and commands that way.
func codeSpans(text string) template.HTML {
	parts := strings.Split(text, "`")
	if len(parts) < 3 {
		return template.HTML(template.HTMLEscapeString(text))
	}
	var b strings.Builder
	for i, part := range parts {
		switch {
		case i%2 == 0:
			b.WriteString(template.HTMLEscapeString(part))
		case i < len(parts)-1:
			b.WriteString(`<code class="font-mono">` + template.HTMLEscapeString(part) + `</code>`)
		default:
			// A backtick that opens no span is a backtick.
			b.WriteString("`" + template.HTMLEscapeString(part))
		}
	}
	return template.HTML(b.String())
}

// shareText says what part of a whole something is, in whole percents. A part that happened is
// never 0%, and one that did not happen every time is never 100%.
func shareText(part, whole int) string {
	if whole <= 0 || part < 0 {
		return ""
	}
	percent := (part*200 + whole) / (whole * 2)
	switch {
	case part > 0 && percent == 0:
		return "under 1%"
	case part < whole && percent >= 100:
		return "over 99%"
	}
	return fmt.Sprintf("%d%%", percent)
}

// tookText says how long something took, from the instant it started to the one it ended. With no
// end it is still going on, and the time is the one elapsed when the page was read.
func tookText(start, end string, readAt time.Time) string {
	from, ok := parseWhen(start)
	if !ok {
		return "–"
	}
	to := readAt
	if t, ok := parseWhen(end); ok {
		to = t
	}
	if to.IsZero() {
		to = time.Now()
	}
	// The instants of the store are told to the second: less than one is not a number of milliseconds.
	if d := to.Sub(from); d >= time.Second {
		return durationText(d)
	}
	return "under 1s"
}

// cycleTone is the color of how a cycle ended. A cycle records its outcome in its own words, so
// the words are shown as they are and only the color is chosen here.
func cycleTone(outcome string) string {
	switch {
	case outcome == "done":
		return "success"
	case strings.HasPrefix(outcome, "running"):
		return "info"
	case outcome == "scope violation", outcome == "preflight failed", outcome == "failed":
		return "error"
	case outcome == "paused", outcome == "stopped before the end":
		return "warning"
	}
	return ""
}

// countText says how many there are of something, with the word for one or for several: "1 change",
// "5 changes", "0 changes".
func countText(n int, one, several string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, several)
}

// firstLine returns the first line of text that is not blank, cut to a length a list can show.
func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return truncateRunes(line, 140)
		}
	}
	return ""
}
