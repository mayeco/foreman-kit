package memory

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run the agent-memory on PATH: it is a program of its own and foreman-kit has no copy
// of it to test against. Without it they fail with what Require says, which is how to install it.

func newMemory(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	created, err := Init(context.Background(), root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !created || !Present(root) {
		t.Fatalf("Init created=%v, Present=%v: want a new memory", created, Present(root))
	}
	return root
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestRequireSaysHowToInstallWhatIsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := Require()
	if err == nil {
		t.Fatal("Require found agent-memory on an empty PATH")
	}
	for _, want := range []string{"agent-memory is not installed", "go install github.com/xChuCx/agent-memory/cmd/agent-memory@latest", "npm install -g @xchucx/agent-memory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Require says %q, without %q", err, want)
		}
	}
	if _, initErr := Init(context.Background(), t.TempDir(), "demo"); initErr == nil || !strings.Contains(initErr.Error(), "is not installed") {
		t.Errorf("Init without agent-memory: %v, want the same refusal", initErr)
	}
}

func TestInitLeavesAMemoryThatExistsAsItIs(t *testing.T) {
	root := newMemory(t)
	if err := Propose(context.Background(), root, Proposal{Intent: "add_pitfall", Op: "append_to_section", Path: "pitfalls.md", SectionID: "pitfalls", Content: "- **Kept**: a lesson.\n"}); err != nil {
		t.Fatal(err)
	}
	created, err := Init(context.Background(), root, "another-name")
	if err != nil || created {
		t.Fatalf("second Init: created=%v err=%v, want it left alone", created, err)
	}
	if !strings.Contains(read(t, root, PitfallsFile), "**Kept**") {
		t.Error("the second Init lost what the memory held")
	}
}

func TestFilesAreTheOnesAgentsReadAndNothingDerived(t *testing.T) {
	root := newMemory(t)
	ctx := context.Background()
	if err := Propose(ctx, root, Proposal{Intent: "refresh_module", Op: "create_file", Path: "modules/repo-web.md",
		Content: "# Repository web\n<!-- @id: repo-web -->\n\n- **Build**: pnpm.\n", Source: "file:repos/web/package.json", Confidence: "confirmed"}); err != nil {
		t.Fatal(err)
	}
	// What one checkout keeps for itself, and what agent-memory derives, is not memory.
	for _, rel := range []string{Dir + "/local/current.main.md", Dir + "/sessions/s.md", Dir + "/staging/x/files/pitfalls.md"} {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("# working notes\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := strings.Join(Files(root), " ")
	want := strings.Join([]string{ConventionsFile, DecisionsFile, RepoModule("web"), PitfallsFile}, " ")
	if got != want {
		t.Errorf("Files:\n got %s\nwant %s", got, want)
	}
	if mods := Modules(root); len(mods) != 1 || mods[0] != RepoModule("web") {
		t.Errorf("Modules = %v", mods)
	}
	if staged := Staged(root); len(staged) != 1 || staged[0] != "x" {
		t.Errorf("Staged = %v, want the one directory under staging/", staged)
	}
}

func TestFingerprintFollowsTheMemoryAndNotItsIndex(t *testing.T) {
	root := newMemory(t)
	before := Fingerprint(root)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(IndexFile)), []byte("# rewritten index\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Fingerprint(root); got != before {
		t.Errorf("the fingerprint changed with index.md alone: %s -> %s", before, got)
	}
	if err := Propose(context.Background(), root, Proposal{Intent: "add_pitfall", Op: "append_to_section", Path: "pitfalls.md", SectionID: "pitfalls", Content: "- **New**: lesson.\n"}); err != nil {
		t.Fatal(err)
	}
	if got := Fingerprint(root); got == before || len(got) != 12 {
		t.Errorf("the fingerprint after a new lesson is %q (before %q)", got, before)
	}
}

func TestProposeWritesEveryKindTheKitUses(t *testing.T) {
	root := newMemory(t)
	ctx := context.Background()
	proposals := []Proposal{
		{Intent: "update_conventions", Op: "append_to_section", Path: "conventions.md", SectionID: "conventions",
			Content: "- **Language**: English.\n", Source: "file:AGENTS.md", Confidence: "confirmed"},
		{Intent: "add_pitfall", Op: "append_to_section", Path: "pitfalls.md", SectionID: "pitfalls",
			Content: "- **Port in use**: stop the dev server first (add-search).\n"},
		{Intent: "record_decision", Op: "append_section", Path: "decisions.md", ParentSectionID: "decisions", Heading: "Use Postgres", HeadingLevel: 2,
			Content: "## Use Postgres\n<!-- @id: use-postgres -->\n\n**Date:** 2026-11-11\n**Status:** active\n**Confidence:** confirmed\n\nThe catalog needs joins.\n",
			Source:  "user:design-review", Confidence: "confirmed"},
		{Intent: "refresh_module", Op: "create_file", Path: "modules/spec-search.md",
			Content: "# Capability search\n<!-- @id: spec-search -->\n\n- **Index**: rebuilt on deploy.\n", Source: "file:openspec/specs/search/spec.md", Confidence: "confirmed"},
	}
	for _, p := range proposals {
		if err := Propose(ctx, root, p); err != nil {
			t.Fatalf("%s %s: %v", p.Intent, p.Path, err)
		}
	}
	for rel, want := range map[string]string{
		ConventionsFile:      "**Language**: English.",
		PitfallsFile:         "**Port in use**",
		DecisionsFile:        "## Use Postgres",
		SpecModule("search"): "**Index**: rebuilt on deploy.",
	} {
		if !strings.Contains(read(t, root, rel), want) {
			t.Errorf("%s does not hold %q", rel, want)
		}
	}
	if staged := Staged(root); len(staged) != 0 {
		t.Errorf("proposals were left staged: %v", staged)
	}
}

func TestProposeReportsWhyAgentMemoryRefuses(t *testing.T) {
	root := newMemory(t)
	before := read(t, root, DecisionsFile)
	// A decision without the fields agent-memory requires of one.
	err := Propose(context.Background(), root, Proposal{Intent: "record_decision", Op: "append_section", Path: "decisions.md", ParentSectionID: "decisions",
		Heading: "Undated", HeadingLevel: 2, Content: "## Undated\n<!-- @id: undated -->\n\nNo fields.\n", Source: "user:review", Confidence: "confirmed"})
	if err == nil {
		t.Fatal("agent-memory took a decision without its fields")
	}
	for _, want := range []string{"did not write .agent-memory/decisions.md", "rejected", "required field missing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal says %q, without %q", err, want)
		}
	}
	if read(t, root, DecisionsFile) != before {
		t.Error("a refused proposal changed decisions.md")
	}
}

func TestMerge3WayKeepsSectionsOfBothSidesAndSaysWhenItCannot(t *testing.T) {
	ctx := context.Background()
	base := "# Decisions\n<!-- @id: decisions -->\n\nintro\n"
	ours := base + "\n## A\n<!-- @id: a -->\n\nours\n"
	theirs := base + "\n## B\n<!-- @id: b -->\n\ntheirs\n"
	merged, clean, err := Merge3Way(ctx, DecisionsFile, base, ours, theirs)
	if err != nil || !clean {
		t.Fatalf("sections added on each side: clean=%v err=%v", clean, err)
	}
	if !strings.Contains(merged, "## A") || !strings.Contains(merged, "## B") {
		t.Errorf("the merge lost a section:\n%s", merged)
	}

	// Both sides added a line to the same section: agent-memory cannot say which holds.
	ours = "# Pitfalls\n<!-- @id: pitfalls -->\n\n- one\n- ours\n"
	theirs = "# Pitfalls\n<!-- @id: pitfalls -->\n\n- one\n- theirs\n"
	merged, clean, err = Merge3Way(ctx, PitfallsFile, "# Pitfalls\n<!-- @id: pitfalls -->\n\n- one\n", ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if clean || merged != "" {
		t.Errorf("one section changed on both sides: clean=%v merged=%q, want the caller told to merge another way", clean, merged)
	}
}

func TestNormalizeWritesTheSummaryAgain(t *testing.T) {
	root := newMemory(t)
	index := filepath.Join(root, filepath.FromSlash(IndexFile))
	want := read(t, root, IndexFile)
	if err := os.WriteFile(index, []byte("merged by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Normalize(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, IndexFile); got != want {
		t.Errorf("index.md after the rebuild:\n%s\nwant:\n%s", got, want)
	}
}

func TestPromptBlockNamesTheNotesOfTheChangeFirst(t *testing.T) {
	root := t.TempDir()
	if got := PromptBlock(root, []string{"search"}); got != "" {
		t.Errorf("without notes the block is %q, want nothing", got)
	}
	for _, rel := range []string{RepoModule("web"), SpecModule("search"), SpecModule("billing"), ModulesDir + "/glossary.md"} {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("# notes\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := PromptBlock(root, []string{"search", "search", "unknown"})
	want := "\nNotes that hold for one repository or one capability are in .agent-memory/modules/:\n" +
		"- read these before you start, for the capabilities this change modifies: .agent-memory/modules/spec-search.md\n" +
		"- open the ones of the repositories you change: .agent-memory/modules/repo-web.md\n" +
		"- open the ones of any other capability your change touches: .agent-memory/modules/spec-billing.md\n" +
		"- also: .agent-memory/modules/glossary.md"
	if got != want {
		t.Errorf("PromptBlock:\n%s\nwant:\n%s", got, want)
	}
}

func TestCapsAndTheLineOfThePlan(t *testing.T) {
	root := newMemory(t)
	long := filepath.Join(root, filepath.FromSlash(RepoModule("web")))
	if err := os.MkdirAll(filepath.Dir(long), 0o755); err != nil {
		t.Fatal(err)
	}
	_, scoped := Caps(root)
	if err := os.WriteFile(long, []byte(strings.Repeat("x", scoped+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	// What was archived is out of every reader's way and has no cap.
	archived := filepath.Join(root, filepath.FromSlash(Dir+"/archive/old.md"))
	if err := os.WriteFile(archived, []byte(strings.Repeat("y", scoped*10)), 0o644); err != nil {
		t.Fatal(err)
	}
	over := OverCap(root)
	if len(over) != 1 || over[0].File != RepoModule("web") || over[0].Chars != scoped+1 || over[0].Cap != scoped {
		t.Fatalf("OverCap = %+v, want only the notes of web", over)
	}
	line := PlanLine(root)
	for _, want := range []string{"`agent-memory fetch`", "`agent-memory propose`", "foreman-kit-memory", "Over the cap, trim first: `.agent-memory/modules/repo-web.md`"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line of the plan lacks %q:\n%s", want, line)
		}
	}

	var out, errOut bytes.Buffer
	if code := RunCLI(root, []string{"status"}, &out, &errOut); code != 1 || !strings.Contains(out.String(), "OVER") {
		t.Errorf("memory status: code %d, output %q, want 1 and the file over its cap", code, out.String())
	}
	out.Reset()
	if code := RunCLI(root, []string{"fingerprint"}, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) != Fingerprint(root) {
		t.Errorf("memory fingerprint: code %d, output %q", code, out.String())
	}
	if code := RunCLI(t.TempDir(), []string{"status"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "foreman-kit update") {
		t.Errorf("memory status without a memory: code %d, said %q", code, errOut.String())
	}
}

func TestImportLegacyTakesEveryPartThroughAgentMemory(t *testing.T) {
	root := newMemory(t)
	legacy := "# Memory\n\nCurated long-term memory. `memory.max_chars` caps it.\n\n---\n\n" +
		"## 1. Project Conventions\n\n- **State model**: Git-tracked files (`.foreman-kit/agent_backlog.json`, `MEMORY.md`, `memory/`) carry state.\n\n---\n\n" +
		"## 2. Lessons and Environment Quirks\n\n<!-- The cycle folds lessons here. -->\n\n- **Flaky login screen**: wait for the form (add-login).\n\n---\n\n" +
		"## 3. Decisions\n\n- **Sessions in cookies**: no server store to operate.\n\n" +
		"## Glossary\n\n- **Umbrella**: the repository of the specifications.\n"
	if err := os.WriteFile(filepath.Join(root, LegacyFile), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, text := range map[string]string{
		"memory/repos/news.md":   "# repos/news\n\nLessons of `repos/news`.\n\n## Testing\n\n- **Data only**: run the whole suite (fix-locale).\n",
		"memory/specs/search.md": "- **Index**: rebuilt on deploy (add-search).\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !LegacyPresent(root) {
		t.Fatal("LegacyPresent does not see MEMORY.md")
	}
	notes, err := ImportLegacy(context.Background(), root, time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 6 {
		t.Errorf("ImportLegacy reports %d parts, want 6:\n%s", len(notes), strings.Join(notes, "\n"))
	}
	conventions := read(t, root, ConventionsFile)
	if !strings.Contains(conventions, "`.foreman-kit/agent_backlog.json`, `.agent-memory/`) carry state.") {
		t.Errorf("conventions.md did not take the conventions with the new name of the memory:\n%s", conventions)
	}
	pitfalls := read(t, root, PitfallsFile)
	for _, want := range []string{"**Flaky login screen**", "Glossary:", "**Umbrella**"} {
		if !strings.Contains(pitfalls, want) {
			t.Errorf("pitfalls.md lacks %q:\n%s", want, pitfalls)
		}
	}
	if strings.Contains(pitfalls, "The cycle folds lessons here") || strings.Contains(pitfalls, "Curated long-term memory") {
		t.Errorf("pitfalls.md took the template's comment or the file's preamble:\n%s", pitfalls)
	}
	// The memory was made for this import: what agent-memory wrote there to say what goes in each
	// file gave way to what came in, once, and the second part pitfalls.md took was added to the first.
	if strings.Contains(pitfalls, "Known traps and recurring failures") || strings.Contains(conventions, "Add project-wide working rules here") {
		t.Errorf("the lines agent-memory starts a file with were kept beside what was taken in:\n%s\n%s", conventions, pitfalls)
	}
	decisions := read(t, root, DecisionsFile)
	for _, want := range []string{"## Decisions recorded before agent-memory", "**Date:** 2026-11-11", "**Status:** active", "**Sessions in cookies**"} {
		if !strings.Contains(decisions, want) {
			t.Errorf("decisions.md lacks %q:\n%s", want, decisions)
		}
	}
	news := read(t, root, RepoModule("news"))
	for _, want := range []string{"# Repository news\n<!-- @id: repo-news -->", "## Testing\n<!-- @id: repo-news-testing -->", "**Data only**"} {
		if !strings.Contains(news, want) {
			t.Errorf("%s lacks %q:\n%s", RepoModule("news"), want, news)
		}
	}
	if strings.Contains(news, "# repos/news") {
		t.Errorf("the notes of news kept the title of the old file:\n%s", news)
	}
	if !strings.Contains(read(t, root, SpecModule("search")), "# Capability search\n<!-- @id: spec-search -->") {
		t.Errorf("%s has no title of its own", SpecModule("search"))
	}
	// The files it took are still there: removing them is the caller's, once all of it is in.
	if !LegacyPresent(root) {
		t.Error("ImportLegacy removed the files it read")
	}
}

func TestImportLegacyAddsToAMemoryThatAlreadyHeldSomething(t *testing.T) {
	root := newMemory(t)
	ctx := context.Background()
	if err := Propose(ctx, root, Proposal{Intent: "add_pitfall", Op: "append_to_section", Path: "pitfalls.md", SectionID: "pitfalls", Content: "- **Already here**: a lesson.\n"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, LegacyFile), []byte("# Memory\n\n## 2. Lessons and Environment Quirks\n\n- **From before**: another lesson.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportLegacy(ctx, root, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	pitfalls := read(t, root, PitfallsFile)
	if !strings.Contains(pitfalls, "**Already here**") || !strings.Contains(pitfalls, "**From before**") {
		t.Errorf("the import did not add to what the memory held:\n%s", pitfalls)
	}
}
