package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"github.com/mayeco/foreman-kit/internal/assets"
)

// The banner greets a person at a terminal. It is written to stderr and only when stderr is a
// terminal: what a script, a pipe, an agent or a log reads is never touched by it. NO_COLOR keeps it
// plain, TERM=dumb leaves it out.

// bannerArt is FOREMAN in the ANSI Shadow figlet font.
var bannerArt = []string{
	"███████╗ ██████╗ ██████╗ ███████╗███╗   ███╗ █████╗ ███╗   ██╗",
	"██╔════╝██╔═══██╗██╔══██╗██╔════╝████╗ ████║██╔══██╗████╗  ██║",
	"█████╗  ██║   ██║██████╔╝█████╗  ██╔████╔██║███████║██╔██╗ ██║",
	"██╔══╝  ██║   ██║██╔══██╗██╔══╝  ██║╚██╔╝██║██╔══██║██║╚██╗██║",
	"██║     ╚██████╔╝██║  ██║███████╗██║ ╚═╝ ██║██║  ██║██║ ╚████║",
	"╚═╝      ╚═════╝ ╚═╝  ╚═╝╚══════╝╚═╝     ╚═╝╚═╝  ╚═╝╚═╝  ╚═══╝",
}

const bannerTagline = "the agents build · the foreman runs the site"

// rgb is a color of the banner's gradient.
type rgb struct{ r, g, b int }

// The gradient runs from the gold of a championship belt to the red of a glove.
var bannerStops = []rgb{{255, 214, 10}, {255, 140, 0}, {240, 60, 40}, {200, 16, 46}}

// bannerStyle says how this terminal shows the banner.
type bannerStyle struct {
	show      bool
	color     bool
	truecolor bool
	width     int
}

func styleFor(w io.Writer) bannerStyle {
	f, ok := w.(*os.File)
	if !ok || !isTerminal(f) || os.Getenv("TERM") == "dumb" {
		return bannerStyle{}
	}
	ct := strings.ToLower(os.Getenv("COLORTERM"))
	return bannerStyle{
		show:      true,
		color:     os.Getenv("NO_COLOR") == "",
		truecolor: ct == "truecolor" || ct == "24bit",
		width:     terminalWidth(f),
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func terminalWidth(f *os.File) int {
	var ws struct{ row, col, x, y uint16 }
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws))); errno == 0 && ws.col > 0 {
		return int(ws.col)
	}
	return 80
}

// at is the gradient's color at t in [0, 1].
func at(t float64) rgb {
	if t <= 0 {
		return bannerStops[0]
	}
	if t >= 1 {
		return bannerStops[len(bannerStops)-1]
	}
	seg := t * float64(len(bannerStops)-1)
	i := int(seg)
	f := seg - float64(i)
	a, b := bannerStops[i], bannerStops[i+1]
	mix := func(x, y int) int { return x + int(float64(y-x)*f+0.5) }
	return rgb{mix(a.r, b.r), mix(a.g, b.g), mix(a.b, b.b)}
}

func (s bannerStyle) fg(c rgb) string {
	if !s.color {
		return ""
	}
	if s.truecolor {
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b)
	}
	// The 6x6x6 cube of 256-color terminals.
	q := func(v int) int { return (v*5 + 127) / 255 }
	return fmt.Sprintf("\x1b[38;5;%dm", 16+36*q(c.r)+6*q(c.g)+q(c.b))
}

func (s bannerStyle) paint(code, text string) string {
	if !s.color {
		return text
	}
	return code + text + "\x1b[0m"
}

func (s bannerStyle) bold(text string) string  { return s.paint("\x1b[1m", text) }
func (s bannerStyle) dim(text string) string   { return s.paint("\x1b[2m", text) }
func (s bannerStyle) gold(text string) string  { return s.paint(s.fg(bannerStops[0]), text) }
func (s bannerStyle) red(text string) string   { return s.paint(s.fg(bannerStops[3]), text) }
func (s bannerStyle) green(text string) string { return s.paint("\x1b[38;5;78m", text) }

// gradient paints text from the first color of the gradient to the last, by column.
func (s bannerStyle) gradient(text string, shift float64) string {
	if !s.color {
		return text
	}
	runes := []rune(text)
	var b strings.Builder
	for i, r := range runes {
		if r == ' ' {
			b.WriteRune(r)
			continue
		}
		t := float64(i)/float64(max(len(runes)-1, 1))*0.85 + shift
		b.WriteString(s.fg(at(t)))
		b.WriteRune(r)
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

func versionLine() string {
	v := "v" + strings.TrimPrefix(assets.Version, "v")
	if c := assets.SourceCommit; c != "" && c != "unknown" && c != "HEAD" {
		if len(c) > 7 {
			c = c[:7]
		}
		v += " (" + c + ")"
	}
	return v
}

// writeBanner writes the large banner: the art, the version and the tagline. A terminal too narrow
// for the art gets the one-line banner instead.
func writeBanner(w io.Writer) {
	s := styleFor(w)
	if !s.show {
		return
	}
	if s.width < len([]rune(bannerArt[0]))+4 {
		writeBannerLine(w, "")
		return
	}
	fmt.Fprintln(w)
	for i, line := range bannerArt {
		// Each row starts a little further along the gradient: the letters shade from top to bottom.
		fmt.Fprintln(w, "  "+s.gradient(line, float64(i)*0.03))
	}
	fmt.Fprintf(w, "  %s %s  %s  %s\n\n", "🥊", s.bold(s.gold("foreman-kit")), s.dim(versionLine()), s.dim("· "+bannerTagline))
}

// writeBannerLine writes the one-line banner a command opens with: the name, the version and what
// runs.
func writeBannerLine(w io.Writer, command string) {
	s := styleFor(w)
	if !s.show {
		return
	}
	line := "🥊 " + s.bold(s.gradient("FOREMAN", 0)) + " " + s.dim(versionLine())
	if command != "" {
		line += s.dim(" · ") + s.gold(command)
	}
	fmt.Fprintln(w, line)
}

// bannerRow is one row of the block the foreground daemon writes when it is up.
type bannerRow struct{ label, value string }

// writeServing writes, below the banner, where the running daemon is: the application, the portal
// it opened, its log and how to stop it.
func writeServing(w io.Writer, rows []bannerRow) {
	s := styleFor(w)
	if !s.show {
		for _, r := range rows {
			fmt.Fprintf(w, "foreman-kit: %s: %s\n", r.label, r.value)
		}
		return
	}
	width := 0
	for _, r := range rows {
		width = max(width, len(r.label))
	}
	for _, r := range rows {
		fmt.Fprintf(w, "  %s %s  %s\n", s.red("▸"), s.dim(r.label+strings.Repeat(" ", width-len(r.label))), r.value)
	}
	fmt.Fprintln(w)
}

// quietCommands open with no banner: what they print is read by a program, or they are the process
// a daemon or an agent starts.
var quietCommands = map[string]bool{
	"exec": true, "agent-exec": true, "completion": true, "__complete": true, "__completeNoDesc": true,
	"evaluator-contract": true, "settings": true, "cycle-prompt": true, "completed-since": true,
	"path": true, "db": true, "version": true,
}

// wantsBanner reports whether the command named by path (the words after foreman-kit) opens with
// the banner.
func wantsBanner(path []string, args []string) bool {
	if len(path) > 0 && quietCommands[path[0]] {
		return false
	}
	for _, a := range args {
		if a == "--json" || strings.HasPrefix(a, "--json=") || a == "--quiet" || a == "-q" {
			return false
		}
	}
	return true
}
