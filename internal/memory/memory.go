// Package memory is the application's long-term memory: the Markdown files of .agent-memory/, kept
// with agent-memory (https://github.com/xChuCx/agent-memory).
//
// agent-memory is a program of its own. foreman-kit runs it and holds none of it: an application
// is given its memory by `agent-memory init`, agents read it with `agent-memory fetch` and write it
// with `agent-memory propose`, and where foreman-kit itself needs the program (to create the
// memory, to take in what an application kept before it, to merge two versions of a file by
// section) it runs the one on PATH. A machine without it is told how to install it, and the command
// that needed it fails.
//
// What foreman-kit reads by itself is only what any reader of a checkout can: which files the
// memory holds and how long they are. This package is the one place that knows either.
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mayeco/foreman-kit/internal/settings"
)

const (
	// Binary is the program that keeps the memory, looked up on PATH.
	Binary = "agent-memory"
	// Dir is the directory of the memory, relative to the application's root.
	Dir = ".agent-memory"
	// ConventionsFile holds the rules that apply to every change.
	ConventionsFile = Dir + "/conventions.md"
	// PitfallsFile holds the lessons and the quirks of the environment that apply to every change.
	PitfallsFile = Dir + "/pitfalls.md"
	// DecisionsFile holds the decisions that are not visible in the code, each with its reason.
	DecisionsFile = Dir + "/decisions.md"
	// ModulesDir holds the notes of one repository or one capability each.
	ModulesDir = Dir + "/modules"
	// IndexFile is the summary agent-memory writes of the other files. Nobody else writes it.
	IndexFile = Dir + "/index.md"

	manifestFile = Dir + "/meta/manifest.yaml"
	stagingDir   = Dir + "/staging"
)

// InstallHelp says how to install agent-memory. It is printed whenever the program is missing.
const InstallHelp = `  go install github.com/xChuCx/agent-memory/cmd/agent-memory@latest
  without Go: npm install -g @xchucx/agent-memory, or a binary of
  https://github.com/xChuCx/agent-memory/releases/latest placed on PATH`

// Require reports whether agent-memory can be run, and how to install it when it cannot.
func Require() error {
	if _, err := exec.LookPath(Binary); err != nil {
		return fmt.Errorf("%s is not installed. foreman-kit keeps the application's memory with it, as a program of its own. Install it, then run this again:\n%s", Binary, InstallHelp)
	}
	return nil
}

// Present reports whether the application at root has a memory.
func Present(root string) bool {
	st, err := os.Stat(filepath.Join(root, filepath.FromSlash(manifestFile)))
	return err == nil && !st.IsDir()
}

// RepoModule returns the file of the notes of the repository checked out at repos/<name>.
func RepoModule(name string) string {
	return ModulesDir + "/repo-" + name + ".md"
}

// SpecModule returns the file of the notes of one capability of openspec/specs/.
func SpecModule(capability string) string {
	return ModulesDir + "/spec-" + capability + ".md"
}

// Inside reports whether rel, a path relative to the application's root, belongs to the memory.
func Inside(rel string) bool {
	rel = filepath.ToSlash(strings.TrimPrefix(rel, "./"))
	return rel == Dir || strings.HasPrefix(rel, Dir+"/")
}

// working names the directories of .agent-memory/ that are not memory: what one checkout keeps for
// itself (notes of a branch, of a session, proposals waiting for review) and what agent-memory
// derives from the rest (its search index). Git does not track them.
var working = map[string]bool{"local": true, "sessions": true, "staging": true, "meta": true}

// Files returns the Markdown files of the memory under root, relative to root and sorted: the ones
// agents read, without the index agent-memory derives from them.
func Files(root string) []string {
	base := filepath.Join(root, filepath.FromSlash(Dir))
	out := []string{}
	_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if rel, relErr := filepath.Rel(base, p); relErr == nil && working[filepath.ToSlash(rel)] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		if rel = filepath.ToSlash(rel); rel != IndexFile {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// Modules returns the notes of one repository or capability each, relative to root and sorted.
func Modules(root string) []string {
	out := []string{}
	for _, f := range Files(root) {
		if strings.HasPrefix(f, ModulesDir+"/") {
			out = append(out, f)
		}
	}
	return out
}

// Size is how many characters a file of the memory holds, and how many it may.
type Size struct {
	File  string `json:"file"`
	Chars int    `json:"chars"`
	Cap   int    `json:"cap"`
}

// Caps returns how many characters a file read by every change may hold, and how many the notes of
// one repository or capability may (cycle.memory.max_chars and cycle.memory.max_chars_per_scope).
func Caps(root string) (shared, scoped int) {
	r := settings.At(root)
	return r.Int(settings.MemoryMaxChars), r.Int(settings.MemoryMaxCharsPerScope)
}

// Sizes returns every file of the memory that has a cap, with its length. What was archived has
// none: it is out of the way of every reader until a search asks for it.
func Sizes(root string) []Size {
	shared, scoped := Caps(root)
	out := []Size{}
	for _, f := range Files(root) {
		if strings.HasPrefix(f, Dir+"/archive/") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			continue
		}
		limit := shared
		if strings.HasPrefix(f, ModulesDir+"/") {
			limit = scoped
		}
		out = append(out, Size{File: f, Chars: utf8.RuneCount(raw), Cap: limit})
	}
	return out
}

// OverCap returns the files of the memory that hold more characters than they may.
func OverCap(root string) []Size {
	out := []Size{}
	for _, s := range Sizes(root) {
		if s.Chars > s.Cap {
			out = append(out, s)
		}
	}
	return out
}

// Fingerprint returns twelve characters that change whenever a file of the memory does: the version
// of the memory an agent was started with.
func Fingerprint(root string) string {
	h := sha256.New()
	for _, f := range Files(root) {
		raw, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		_, _ = h.Write([]byte(f))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(raw)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// PromptBlock returns what the PLAN and BUILD prompts add about the notes of one repository or
// capability: which ones exist, those of the capabilities the change modifies first. It is empty
// while there are none. It reads names of files and runs nothing.
func PromptBlock(root string, capabilities []string) string {
	files := Modules(root)
	if len(files) == 0 {
		return ""
	}
	exists := make(map[string]bool, len(files))
	for _, f := range files {
		exists[f] = true
	}
	var own []string
	isOwn := make(map[string]bool)
	for _, c := range capabilities {
		if f := SpecModule(c); exists[f] && !isOwn[f] {
			isOwn[f] = true
			own = append(own, f)
		}
	}
	var repos, specs, other []string
	for _, f := range files {
		switch {
		case isOwn[f]:
		case strings.HasPrefix(f, ModulesDir+"/repo-"):
			repos = append(repos, f)
		case strings.HasPrefix(f, ModulesDir+"/spec-"):
			specs = append(specs, f)
		default:
			other = append(other, f)
		}
	}
	lines := []string{fmt.Sprintf("\nNotes that hold for one repository or one capability are in %s/:", ModulesDir)}
	if len(own) > 0 {
		lines = append(lines, "- read these before you start, for the capabilities this change modifies: "+strings.Join(own, ", "))
	}
	if len(repos) > 0 {
		lines = append(lines, "- open the ones of the repositories you change: "+strings.Join(repos, ", "))
	}
	if len(specs) > 0 {
		lines = append(lines, "- open the ones of any other capability your change touches: "+strings.Join(specs, ", "))
	}
	if len(other) > 0 {
		lines = append(lines, "- also: "+strings.Join(other, ", "))
	}
	return strings.Join(lines, "\n")
}

// PlanLine returns what the plan of a cycle or of a visual session says about the memory: where it
// is, how it is read and written, how full each file is, and which ones to trim first.
func PlanLine(root string) string {
	shared, scoped := Caps(root)
	chars := map[string]int{}
	for _, s := range Sizes(root) {
		chars[s.File] = s.Chars
	}
	line := fmt.Sprintf("`%s/`, read with `%s fetch` and written with `%s propose` (the skill `foreman-kit-memory` has the commands). For what holds for every change: `conventions.md` (%d of %d characters), `pitfalls.md` (%d of %d) and `decisions.md` (%d of %d). For what holds for one repository or capability, read only by the changes that touch it: `modules/repo-<name>.md` and `modules/spec-<capability>.md` (%d each).",
		Dir, Binary, Binary,
		chars[ConventionsFile], shared, chars[PitfallsFile], shared, chars[DecisionsFile], shared, scoped)
	over := OverCap(root)
	if len(over) == 0 {
		return line
	}
	parts := make([]string, 0, len(over))
	for _, o := range over {
		parts = append(parts, fmt.Sprintf("`%s` (%d of %d)", o.File, o.Chars, o.Cap))
	}
	return fmt.Sprintf("%s Over the cap, trim first: %s. Move what holds for one scope into its notes, merge duplicates, and drop what no longer holds or is never acted on.",
		line, strings.Join(parts, ", "))
}

// Staged returns the proposals waiting for review in the checkout at root, oldest first. A proposal
// agent-memory staged was not written to the memory: it stays in that checkout, which git does not
// track, until somebody applies it there.
func Staged(root string) []string {
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(stagingDir)))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// RunCLI implements `foreman-kit memory status | fingerprint`.
func RunCLI(root string, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if len(args) == 1 && args[0] == "status" {
		if !Present(root) {
			fmt.Fprintf(stderr, "%s/ is missing: `foreman-kit update` creates it\n", Dir)
			return 1
		}
		over := false
		for _, s := range Sizes(root) {
			mark := ""
			if s.Chars > s.Cap {
				mark, over = "  OVER", true
			}
			fmt.Fprintf(stdout, "%-48s %7d of %d%s\n", s.File, s.Chars, s.Cap, mark)
		}
		if staged := Staged(root); len(staged) > 0 {
			fmt.Fprintf(stdout, "%d proposal(s) staged here and not applied: %s\n", len(staged), strings.Join(staged, ", "))
		}
		if over {
			return 1
		}
		return 0
	}
	if len(args) == 1 && args[0] == "fingerprint" {
		fmt.Fprintln(stdout, Fingerprint(root))
		return 0
	}
	fmt.Fprintln(stderr, "Usage: foreman-kit memory status | fingerprint")
	return 2
}
