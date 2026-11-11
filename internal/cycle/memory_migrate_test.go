package cycle

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/store"
)

// These tests run the agent-memory and the openspec on PATH, as an application's install does: the
// kit holds no copy of either. On a machine without them they fail with how to install them.

// agentMemory runs agent-memory in dir and returns what it printed.
func agentMemory(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(memory.Binary, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", memory.Binary, args, err, out)
	}
	return string(out)
}

// before returns a seed as the kit that kept the memory in MEMORY.md and memory/ shipped it.
func before(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "before_agent_memory", strings.ReplaceAll(rel, "/", "_")))
	if err != nil {
		t.Fatal(err)
	}
	// Those seeds are of the kit before it was called Foreman too: an application of then has its
	// former names rewritten by the update before its memory moves (moveToForeman).
	return RewriteFormerNames(string(raw))
}

func TestInitSaysHowToInstallWhatItRunsAndWritesNothing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("FOREMANKIT_FORCE_BUILTIN", "")
	target := filepath.Join(t.TempDir(), "app")
	err := Init(context.Background(), target, KitOptions{Agents: "claude"}, io.Discard)
	if err == nil {
		t.Fatal("Init installed an application on a machine with neither agent-memory nor openspec")
	}
	for _, want := range []string{
		"agent-memory is not installed", "go install github.com/xChuCx/agent-memory/cmd/agent-memory@latest",
		"openspec is not installed", "npm install -g @fission-ai/openspec@latest",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Init says:\n%v\nwithout %q", err, want)
		}
	}
	if exists(target) {
		t.Errorf("Init wrote in %s before it found out what was missing", target)
	}
}

func TestInitRunsOpenSpecAndAgentMemory(t *testing.T) {
	t.Setenv("FOREMANKIT_FORCE_BUILTIN", "")
	ctx := context.Background()
	app := filepath.Join(t.TempDir(), "shop")
	var out bytes.Buffer
	if err := Init(ctx, app, KitOptions{Agents: "claude"}, &out); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// What only `openspec init` leaves: its own skills, beside the kit's.
	if !exists(filepath.Join(app, ".agents", "skills", "openspec-propose")) || !fileExists(filepath.Join(app, "openspec", "config.yaml")) {
		t.Error("Init did not run openspec init: its skills or openspec/config.yaml are missing")
	}
	// What only `agent-memory init` leaves, with the kit's conventions written through it.
	if !memory.Present(app) {
		t.Fatalf("Init did not run agent-memory init: no %s/", memory.Dir)
	}
	if !strings.Contains(out.String(), "agent-memory init created .agent-memory/") {
		t.Errorf("Init does not say it created the memory:\n%s", out.String())
	}
	conventions := readText(t, filepath.Join(app, filepath.FromSlash(memory.ConventionsFile)))
	for _, want := range []string{"**Topology**", "**Memory**", "`.agent-memory/`"} {
		if !strings.Contains(conventions, want) {
			t.Errorf("%s lacks %q:\n%s", memory.ConventionsFile, want, conventions)
		}
	}
	if strings.Contains(conventions, "This comment is not written") {
		t.Errorf("%s took the comment of the template:\n%s", memory.ConventionsFile, conventions)
	}
	if exists(filepath.Join(app, memory.LegacyFile)) {
		t.Errorf("Init still writes %s", memory.LegacyFile)
	}
	if fileExists(filepath.Join(app, ".agents", "skills", "foreman-kit-memory", "SKILL.md")) == false {
		t.Error("the skill foreman-kit-memory was not installed")
	}

	// The memory is committed in the form agent-memory writes it in: a search, or a rebuild of its
	// index, in a checkout that holds that commit changes nothing git tracks.
	initGitRepo(t, app)
	commitAll(t, app, "install")
	agentMemory(t, app, "fetch", "topology")
	agentMemory(t, app, "rebuild-index")
	if dirty := gitRun(t, app, "status", "--porcelain"); dirty != "" {
		t.Errorf("a search and a rebuild changed the memory Init left:\n%s", dirty)
	}

	// A second Init is refused, and the memory is what it was.
	if err := Init(ctx, app, KitOptions{Agents: "claude"}, io.Discard); err == nil {
		t.Error("a second Init was not refused")
	}
}

// TestTheSeedsOfBeforeBecomeTheSeedsOfToday: the sentences `foreman-kit update` rewrites are every
// difference between the seeds that named MEMORY.md and the seeds of today, so an application that
// never edited one of those files holds today's seed after the update and is proposed nothing.
func TestTheSeedsOfBeforeBecomeTheSeedsOfToday(t *testing.T) {
	for _, m := range memoryMentions {
		text := before(t, m.file)
		for _, pair := range m.pairs {
			if !strings.Contains(text, pair[0]) {
				t.Errorf("%s as it was does not hold %q", m.file, pair[0])
			}
			text = strings.ReplaceAll(text, pair[0], pair[1])
		}
		today, err := assets.ReadSeed(m.file)
		if err != nil {
			t.Fatal(err)
		}
		if text != string(today) {
			t.Errorf("%s as it was, rewritten, is not the seed of today", m.file)
		}
		for _, stale := range []string{"MEMORY.md", "memory/repos", "memory/specs", "`memory/`"} {
			if strings.Contains(string(today), stale) {
				t.Errorf("the seed %s still names %s", m.file, stale)
			}
		}
	}
}

// installedBeforeAgentMemory turns a fresh installation into one made by the kit that kept the
// memory in MEMORY.md and memory/: no .agent-memory/, those files with what the application learned,
// the prompts, guides and settings as that kit shipped them, and a lock with their checksums. It
// commits the result.
func installedBeforeAgentMemory(t *testing.T, ctx context.Context, app, lessons string) {
	t.Helper()
	if err := Init(ctx, app, KitOptions{Agents: "claude"}, io.Discard); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(app, filepath.FromSlash(memory.Dir))); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(app, KitLockFile)
	var lock KitLock
	if err := json.Unmarshal([]byte(readText(t, lockPath)), &lock); err != nil {
		t.Fatal(err)
	}
	write := func(rel, text string) {
		t.Helper()
		full := filepath.Join(app, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range memoryMentions {
		text := before(t, m.file)
		write(m.file, text)
		lock.Files[m.file] = LockFileEntry{Class: "seed", SHA256: assets.SHA256Bytes([]byte(text))}
	}
	agents := before(t, agentsFile)
	write(agentsFile, agents)
	lock.Files[agentsFile] = LockFileEntry{Class: "seed", SHA256: assets.SHA256Bytes([]byte(agents))}

	seed := before(t, memory.LegacyFile)
	lock.Files[memory.LegacyFile] = LockFileEntry{Class: "seed", SHA256: assets.SHA256Bytes([]byte(seed))}
	const marker = "<!-- The scheduled cycle folds"
	if !strings.Contains(seed, marker) {
		t.Fatalf("the seed of %s as it was has no place for lessons", memory.LegacyFile)
	}
	write(memory.LegacyFile, strings.Replace(seed, marker, lessons+"\n\n"+marker, 1))
	write("memory/repos/web.md", "# repos/web\n\nLessons of `repos/web`.\n\n## Install\n\n- **Corepack first**: enable it before pnpm install (add-search)\n")

	raw, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write(KitLockFile, string(raw)+"\n")
	initGitRepo(t, app)
	commitAll(t, app, "an application of the kit before agent-memory")
}

func TestUpdateTakesWhatTheApplicationLearnedIntoAgentMemory(t *testing.T) {
	t.Setenv("FOREMANKIT_FORCE_BUILTIN", "1")
	ctx := context.Background()
	app := filepath.Join(t.TempDir(), "shop")
	installedBeforeAgentMemory(t, ctx, app, "- **Flaky login screen**: wait for the wombat form before typing (add-login)")

	var out bytes.Buffer
	if err := Upgrade(ctx, app, KitOptions{}, &out); err != nil {
		t.Fatalf("Upgrade: %v\n%s", err, out.String())
	}
	said := out.String()
	for _, want := range []string{
		"agent-memory init created .agent-memory/",
		`MEMORY.md, "Lessons and Environment Quirks" -> .agent-memory/pitfalls.md`,
		"memory/repos/web.md -> .agent-memory/modules/repo-web.md",
		"MEMORY.md and memory/ were taken into .agent-memory/ and removed",
		".foreman-kit/prompts/build.md: it names the memory as it is kept now",
		".foreman-kit/agents.yaml: the agents that are told which tools they may use may run agent-memory",
	} {
		if !strings.Contains(said, want) {
			t.Errorf("update does not say %q:\n%s", want, said)
		}
	}

	if exists(filepath.Join(app, memory.LegacyFile)) || exists(filepath.Join(app, memory.LegacyDir)) {
		t.Error("MEMORY.md or memory/ is still there after the update")
	}
	pitfalls := readText(t, filepath.Join(app, filepath.FromSlash(memory.PitfallsFile)))
	if !strings.Contains(pitfalls, "**Flaky login screen**") {
		t.Errorf("the lesson of MEMORY.md is not in %s:\n%s", memory.PitfallsFile, pitfalls)
	}
	conventions := readText(t, filepath.Join(app, filepath.FromSlash(memory.ConventionsFile)))
	if !strings.Contains(conventions, "**Topology**") || strings.Contains(conventions, "`MEMORY.md`, `memory/`") {
		t.Errorf("%s did not take the conventions with the new name of the memory:\n%s", memory.ConventionsFile, conventions)
	}
	web := readText(t, filepath.Join(app, filepath.FromSlash(memory.RepoModule("web"))))
	for _, want := range []string{"# Repository web\n<!-- @id: repo-web -->", "## Install\n<!-- @id: repo-web-install -->", "**Corepack first**"} {
		if !strings.Contains(web, want) {
			t.Errorf("%s lacks %q:\n%s", memory.RepoModule("web"), want, web)
		}
	}

	// A file nobody edited is today's seed, and is proposed nothing.
	for _, m := range memoryMentions {
		today, _ := assets.ReadSeed(m.file)
		if got := readText(t, filepath.Join(app, filepath.FromSlash(m.file))); got != string(today) {
			t.Errorf("%s is not the seed of today after the update", m.file)
		}
		if exists(filepath.Join(app, filepath.FromSlash(m.file)) + ".new") {
			t.Errorf("update proposed %s.new for a file it had already brought up to date", m.file)
		}
	}
	if agents := readText(t, filepath.Join(app, agentsFile)); !strings.Contains(agents, allowsAgentMemory) {
		t.Errorf("%s does not let the restricted agents run agent-memory", agentsFile)
	}

	// All of it is in the commit of the update, the removal included.
	if dirty := gitRun(t, app, "status", "--porcelain"); dirty != "" {
		t.Errorf("the update left changes uncommitted:\n%s", dirty)
	}
	tracked := gitRun(t, app, "ls-files")
	if strings.Contains(tracked, "MEMORY.md") || !strings.Contains(tracked, memory.PitfallsFile) || strings.Contains(tracked, "index.sqlite") {
		t.Errorf("the commit of the update tracks the wrong files of the memory:\n%s", tracked)
	}

	// A checkout that receives that commit finds what was taken in, and nothing git tracks changes
	// when it searches or rebuilds its index.
	clone := filepath.Join(t.TempDir(), "clone")
	gitRun(t, filepath.Dir(clone), "clone", "--quiet", app, clone)
	if found := agentMemory(t, clone, "fetch", "wombat"); !strings.Contains(found, "Flaky login screen") {
		t.Errorf("a search in a clone does not find the lesson taken from MEMORY.md:\n%s", found)
	}
	if found := agentMemory(t, clone, "fetch", "corepack"); !strings.Contains(found, "Corepack first") {
		t.Errorf("a search in a clone does not find the lesson taken from memory/repos/web.md:\n%s", found)
	}
	agentMemory(t, clone, "rebuild-index")
	if dirty := gitRun(t, clone, "status", "--porcelain"); dirty != "" {
		t.Errorf("a search and a rebuild changed the memory the update committed:\n%s", dirty)
	}

	// Nothing is left to do, and the checks of the application say nothing about its memory.
	out.Reset()
	if err := Upgrade(ctx, app, KitOptions{}, &out); err != nil || !strings.Contains(out.String(), "nothing to do") {
		t.Errorf("a second update: %v\n%s", err, out.String())
	}
}

func TestUpdateStopsWhenAgentMemoryRefusesAPartAndLeavesTheApplicationAsItWas(t *testing.T) {
	t.Setenv("FOREMANKIT_FORCE_BUILTIN", "1")
	ctx := context.Background()
	app := filepath.Join(t.TempDir(), "shop")
	// Written in two halves: the test holds nothing a scanner of this repository would take for a key.
	secret := "-----BEGIN RSA " + "PRIVATE KEY-----"
	installedBeforeAgentMemory(t, ctx, app, "- **Deploy key**: paste "+secret+" into the form (add-deploy)")
	head := gitRun(t, app, "rev-parse", "HEAD")
	branch := gitRun(t, app, "symbolic-ref", "--short", "HEAD")

	var out bytes.Buffer
	err := Upgrade(ctx, app, KitOptions{}, &out)
	if err == nil {
		t.Fatalf("the update took a secret into the memory:\n%s", out.String())
	}
	for _, want := range []string{`MEMORY.md, section "Lessons and Environment Quirks"`, "agent-memory did not write .agent-memory/pitfalls.md", "secret_detected"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the update says:\n%v\nwithout %q", err, want)
		}
	}
	if !fileExists(filepath.Join(app, memory.LegacyFile)) || !fileExists(filepath.Join(app, "memory", "repos", "web.md")) {
		t.Error("the update removed the old memory although it did not take all of it")
	}
	if exists(filepath.Join(app, filepath.FromSlash(memory.Dir))) {
		t.Errorf("the update left a %s/ with part of the memory in it", memory.Dir)
	}
	if got := gitRun(t, app, "rev-parse", "HEAD"); got != head {
		t.Errorf("the update committed (%s) although it failed", got)
	}
	if got := gitRun(t, app, "symbolic-ref", "--short", "HEAD"); got != branch {
		t.Errorf("the update that failed left the application on branch %s, not on %s where it was", got, branch)
	}
	if dirty := gitRun(t, app, "status", "--porcelain"); dirty != "" {
		t.Errorf("the update that failed left the application changed:\n%s", dirty)
	}
}

func TestUpdateAsksForAgentMemoryBeforeItWritesAnything(t *testing.T) {
	t.Setenv("FOREMANKIT_FORCE_BUILTIN", "1")
	ctx := context.Background()
	app := filepath.Join(t.TempDir(), "shop")
	installedBeforeAgentMemory(t, ctx, app, "- **A lesson**: kept (add-x)")
	branch := gitRun(t, app, "symbolic-ref", "--short", "HEAD")

	// A PATH with git and without agent-memory.
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(gitPath, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	err = Upgrade(ctx, app, KitOptions{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "agent-memory is not installed") || !strings.Contains(err.Error(), "go install github.com/xChuCx/agent-memory") {
		t.Fatalf("update without agent-memory: %v, want how to install it", err)
	}
	if got := gitRun(t, app, "symbolic-ref", "--short", "HEAD"); got != branch {
		t.Errorf("the update moved the application to branch %s before it found agent-memory missing", got)
	}
	if dirty := gitRun(t, app, "status", "--porcelain"); dirty != "" {
		t.Errorf("the update changed the application before it found agent-memory missing:\n%s", dirty)
	}
}

// memoryRepo returns a checkout with a memory made by agent-memory, committed and pushed to the
// bare repository it returns beside it.
func memoryRepo(t *testing.T) (checkout, bare string) {
	t.Helper()
	bare = filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, filepath.Dir(bare), "init", "--quiet", "--bare", "-b", "main", bare)
	checkout = filepath.Join(t.TempDir(), "session")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, checkout, "init", "--quiet", "-b", "main")
	gitRun(t, checkout, "config", "user.name", "t")
	gitRun(t, checkout, "config", "user.email", "t@example.com")
	gitRun(t, checkout, "remote", "add", "origin", bare)
	if _, err := memory.Init(context.Background(), checkout, "demo"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, checkout, "add", "-A")
	gitRun(t, checkout, "commit", "--quiet", "-m", "base")
	gitRun(t, checkout, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	gitRun(t, checkout, "fetch", "--quiet", "origin")
	return checkout, bare
}

// cloneOf returns a clone of bare that can commit.
func cloneOf(t *testing.T, bare, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	gitRun(t, filepath.Dir(dir), "clone", "--quiet", bare, dir)
	gitRun(t, dir, "config", "user.name", "t")
	gitRun(t, dir, "config", "user.email", "t@example.com")
	return dir
}

func pitfall(t *testing.T, dir, line string) {
	t.Helper()
	if err := memory.Propose(context.Background(), dir, memory.Proposal{Intent: "add_pitfall", Op: "append_to_section", Path: "pitfalls.md", SectionID: "pitfalls", Content: line + "\n"}); err != nil {
		t.Fatal(err)
	}
}

// TestARecordedMemoryIsLeftAsItIsByEveryCheckoutThatReceivesIt: the record of a session commits the
// memory in the form agent-memory writes it in, also what the session wrote by hand. A checkout that
// receives the commit, refreshed, finds it, and neither a search nor a rebuild of the index changes
// a file git tracks there: a checkout changed that way would refuse the next fast-forward.
func TestARecordedMemoryIsLeftAsItIsByEveryCheckoutThatReceivesIt(t *testing.T) {
	ctx := context.Background()
	session, bare := memoryRepo(t)
	st, err := store.OpenAt(filepath.Join(t.TempDir(), "state.db"), "ws-memory-record", session)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// A checkout that searched before the session recorded: its index is of the memory as it was.
	reader := cloneOf(t, bare, "reader")
	agentMemory(t, reader, "fetch", "quokka")

	pitfall(t, session, "- **Quokka trap**: never feed it (add-zoo)")
	// Written by hand, without the line agent-memory finds a section by.
	notes := filepath.Join(session, filepath.FromSlash(memory.RepoModule("api")))
	if err := os.MkdirAll(filepath.Dir(notes), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notes, []byte("# Repository api\n\n- **Numbat rule**: paginate every list (add-api)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// And one left staged: it is not written, so it is not recorded, and the record says so.
	agentMemory(t, session, "propose", "--intent", "update_conventions", "--op", "append_to_section", "--path", "conventions.md",
		"--section-id", "conventions", "--source", "file:Makefile", "--content", "- **Staged**: never applied")

	var out bytes.Buffer
	if err := RecordSession(ctx, session, "main", "chore(memory): test session", st, &out); err != nil {
		t.Fatalf("RecordSession: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "1 change(s) to the memory were staged") {
		t.Errorf("the record does not say a proposal was left staged:\n%s", out.String())
	}
	if got := gitRun(t, bare, "show", "refs/heads/main:"+memory.RepoModule("api")); !strings.Contains(got, "<!-- @id:") {
		t.Errorf("the notes written by hand were recorded without the id of their section:\n%s", got)
	}
	if got := gitRun(t, bare, "show", "refs/heads/main:"+memory.IndexFile); !strings.Contains(got, "modules/repo-api.md") {
		t.Errorf("index.md was recorded without the notes of api:\n%s", got)
	}
	if got := gitRun(t, bare, "show", "refs/heads/main:"+memory.ConventionsFile); strings.Contains(got, "never applied") {
		t.Errorf("a proposal that was only staged was recorded:\n%s", got)
	}

	gitRun(t, reader, "pull", "--quiet", "--ff-only")
	if err := memory.Refresh(ctx, reader); err != nil {
		t.Fatal(err)
	}
	for word, want := range map[string]string{"quokka": "Quokka trap", "numbat": "Numbat rule"} {
		if found := agentMemory(t, reader, "fetch", word); !strings.Contains(found, want) {
			t.Errorf("after the refresh a search for %q does not find %q:\n%s", word, want, found)
		}
	}
	agentMemory(t, reader, "rebuild-index")
	if dirty := gitRun(t, reader, "status", "--porcelain"); dirty != "" {
		t.Errorf("a refresh, a search and a rebuild changed the recorded memory in another checkout:\n%s", dirty)
	}
}

// TestRefreshChangesNoTrackedFileEvenWhenTheMemoryWasCommittedByHand: somebody commits notes written
// by hand without rebuilding the index. The refresh before an agent starts leaves the checkout as it
// was: a file changed there would be taken for the agent's work.
func TestRefreshChangesNoTrackedFileEvenWhenTheMemoryWasCommittedByHand(t *testing.T) {
	checkout, _ := memoryRepo(t)
	notes := filepath.Join(checkout, filepath.FromSlash(memory.RepoModule("api")))
	if err := os.MkdirAll(filepath.Dir(notes), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notes, []byte("# Repository api\n\n- **Numbat rule**: paginate every list\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, checkout, "add", "-A")
	gitRun(t, checkout, "commit", "--quiet", "-m", "notes by hand")
	if err := memory.Refresh(context.Background(), checkout); err != nil {
		t.Fatal(err)
	}
	if dirty := gitRun(t, checkout, "status", "--porcelain"); dirty != "" {
		t.Errorf("the refresh changed tracked files:\n%s", dirty)
	}
}

// TestTwoSessionsRecordTheirLessonsAndNeitherIsLost: two sessions write to the memory from the same
// base, both to the one section lessons go to, and one adds a decision. The second to record is made
// again on top of the first: agent-memory merges what it can by section, the lines of the section
// both wrote to are kept from both, and index.md is written again from the result.
func TestTwoSessionsRecordTheirLessonsAndNeitherIsLost(t *testing.T) {
	ctx := context.Background()
	first, bare := memoryRepo(t)
	second := cloneOf(t, bare, "second")
	st, err := store.OpenAt(filepath.Join(t.TempDir(), "state.db"), "ws-memory-two", first)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	pitfall(t, first, "- **From the cycle**: lesson one (add-a)")
	// Notes of a repository: index.md lists them, so both sessions change that file too.
	if err := memory.Propose(ctx, first, memory.Proposal{Intent: "refresh_module", Op: "create_file", Path: "modules/repo-web.md",
		Source: "file:repos/web/package.json", Confidence: "confirmed",
		Content: "# Repository web\n<!-- @id: repo-web -->\n\n- **Install**: corepack first (add-a)\n"}); err != nil {
		t.Fatal(err)
	}
	// A decision in each session: two sections of one file, which agent-memory merges by itself, and
	// one line of index.md that counts them, which no merge of the two versions gets right.
	if err := memory.Propose(ctx, first, memory.Proposal{Intent: "record_decision", Op: "append_section", Path: "decisions.md", ParentSectionID: "decisions",
		Heading: "Use Postgres", HeadingLevel: 2, Source: "user:review", Confidence: "confirmed",
		Content: "## Use Postgres\n<!-- @id: use-postgres -->\n\n**Date:** 2026-11-11\n**Status:** active\n**Confidence:** confirmed\n\nThe catalog needs joins.\n"}); err != nil {
		t.Fatal(err)
	}
	pitfall(t, second, "- **From the visual session**: lesson two (add-b)")
	if err := memory.Propose(ctx, second, memory.Proposal{Intent: "record_decision", Op: "append_section", Path: "decisions.md", ParentSectionID: "decisions",
		Heading: "Sessions in cookies", HeadingLevel: 2, Source: "user:review", Confidence: "confirmed",
		Content: "## Sessions in cookies\n<!-- @id: sessions-in-cookies -->\n\n**Date:** 2026-11-11\n**Status:** active\n**Confidence:** confirmed\n\nNo server store.\n"}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := RecordSession(ctx, first, "main", "chore(memory): cycle", st, &out); err != nil {
		t.Fatalf("the first record: %v\n%s", err, out.String())
	}
	if err := RecordSession(ctx, second, "main", "chore(memory): visual QA", st, &out); err != nil {
		t.Fatalf("the second record: %v\n%s", err, out.String())
	}

	pitfalls := gitRun(t, bare, "show", "refs/heads/main:"+memory.PitfallsFile)
	for _, want := range []string{"**From the cycle**", "**From the visual session**"} {
		if strings.Count(pitfalls, want) != 1 {
			t.Errorf("%s holds %q %d time(s), want once:\n%s", memory.PitfallsFile, want, strings.Count(pitfalls, want), pitfalls)
		}
	}
	if strings.Contains(pitfalls, "merge-conflict") || strings.Contains(pitfalls, "<<<<<<<") {
		t.Errorf("the record published a conflict:\n%s", pitfalls)
	}
	if got := gitRun(t, bare, "show", "refs/heads/main:"+memory.DecisionsFile); !strings.Contains(got, "## Sessions in cookies") || !strings.Contains(got, "## Use Postgres") || strings.Contains(got, "merge-conflict") {
		t.Errorf("the decisions of the two sessions are not both recorded:\n%s", got)
	}
	index := gitRun(t, bare, "show", "refs/heads/main:"+memory.IndexFile)
	if !strings.Contains(index, "2 active") || !strings.Contains(index, "modules/repo-web.md") {
		t.Errorf("index.md is not the summary of what both sessions wrote:\n%s", index)
	}

	clone := cloneOf(t, bare, "clone")
	agentMemory(t, clone, "rebuild-index")
	if dirty := gitRun(t, clone, "status", "--porcelain"); dirty != "" {
		t.Errorf("a rebuild changed the memory the two sessions recorded:\n%s", dirty)
	}
}

// TestARecordLeftFromBeforeAgentMemoryIsKeptAndNotPublished: a session of the kit that kept the
// memory in MEMORY.md left a record commit unpublished, and the kit of today finds it. That file is
// no longer part of what a record may publish, so the commit is kept under a ref and said to be,
// never rebased into something that silently lacks its lessons.
func TestARecordLeftFromBeforeAgentMemoryIsKeptAndNotPublished(t *testing.T) {
	ctx := context.Background()
	session, bare := memoryRepo(t)
	before := gitRun(t, bare, "rev-parse", "refs/heads/main")
	if err := os.WriteFile(filepath.Join(session, memory.LegacyFile), []byte("# Memory\n- a lesson of the old kit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, session, "add", memory.LegacyFile)
	gitRun(t, session, "commit", "--quiet", "-m", "chore(memory): scheduled cycle 20261110T230000Z")

	var out bytes.Buffer
	if PublishLeftoverRecord(ctx, session, "origin/main", "main", &out) {
		t.Fatalf("a record that changes %s was published:\n%s", memory.LegacyFile, out.String())
	}
	if !strings.Contains(out.String(), "outside the recorded scope") || !strings.Contains(out.String(), "refs/openspec/unpublished/") {
		t.Errorf("the kit does not say where it kept the record:\n%s", out.String())
	}
	if got := gitRun(t, bare, "rev-parse", "refs/heads/main"); got != before {
		t.Errorf("the base branch moved from %s to %s", before, got)
	}
	kept := gitRun(t, session, "for-each-ref", "--format=%(refname)", "refs/openspec/unpublished/")
	if kept == "" {
		t.Fatal("the record was not kept under a ref")
	}
	if got := gitRun(t, session, "show", strings.Fields(kept)[0]+":"+memory.LegacyFile); !strings.Contains(got, "a lesson of the old kit") {
		t.Errorf("the ref does not hold the lesson:\n%s", got)
	}
}
