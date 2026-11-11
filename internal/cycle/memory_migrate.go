package cycle

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/memory"
)

// The memory of an application is kept with agent-memory, and its specifications with openspec: two
// programs of their own that foreman-kit runs and does not contain. `foreman-kit init` runs both to
// give a new application its .agent-memory/ and its openspec/, and `foreman-kit update` runs
// agent-memory to give an application installed before it the same, taking in what it kept in
// MEMORY.md and memory/. On a machine that lacks one of them the command says how to install it and
// fails before it writes anything.

// openspecInstallHelp says how to install the openspec CLI.
const openspecInstallHelp = "  npm install -g @fission-ai/openspec@latest"

// requireTools reports the programs foreman-kit runs to install or update an application that are
// not on PATH, each with how to install it. FOREMANKIT_FORCE_BUILTIN=1, which tests and simulations
// set to work without the openspec CLI, leaves openspec out.
func requireTools() error {
	var missing []string
	if err := memory.Require(); err != nil {
		missing = append(missing, err.Error())
	}
	if os.Getenv("FOREMANKIT_FORCE_BUILTIN") != "1" {
		if _, err := exec.LookPath("openspec"); err != nil {
			missing = append(missing, "openspec is not installed. foreman-kit keeps the application's specifications with it, as a program of its own. Install it, then run this again:\n"+openspecInstallHelp)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s", strings.Join(missing, "\n"))
	}
	return nil
}

// memoryConventionsTemplate is the template of the conventions a new application's memory starts
// with: what the kit itself establishes about every application it runs.
const memoryConventionsTemplate = "memory_conventions.md"

// initMemory gives the application at target its memory with `agent-memory init` and writes the
// kit's conventions into it. An application that already has a memory keeps it untouched.
func initMemory(ctx context.Context, target string, out io.Writer) error {
	created, err := memory.Init(ctx, target, memoryName(target))
	if err != nil {
		return err
	}
	if !created {
		return nil
	}
	if err := seedMemoryConventions(ctx, target); err != nil {
		// A memory with nothing of the kit in it is not what the next run should take for done.
		_ = os.RemoveAll(filepath.Join(target, filepath.FromSlash(memory.Dir)))
		return err
	}
	fmt.Fprintf(out, "kit: %s init created %s/\n", memory.Binary, memory.Dir)
	return nil
}

// memoryName names the memory of the application at target after its directory.
func memoryName(target string) string {
	abs, err := filepath.Abs(target)
	if err != nil {
		abs = target
	}
	return filepath.Base(abs)
}

// seedMemoryConventions writes the kit's conventions into the memory of target, through
// agent-memory, and leaves the memory in the form it is committed in.
func seedMemoryConventions(ctx context.Context, target string) error {
	raw, err := assets.ReadTemplate(target, memoryConventionsTemplate)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(HTMLCommentRe.ReplaceAllString(string(raw), ""))
	if text != "" {
		// The memory was just created: these conventions stand where agent-memory wrote what a
		// conventions file is for.
		if err := memory.Propose(ctx, target, memory.Proposal{
			Intent: "update_conventions", Op: "replace_section_content", Path: "conventions.md", SectionID: "conventions",
			Content: "\n" + text + "\n", Source: "file:AGENTS.md", Confidence: "confirmed",
		}); err != nil {
			return err
		}
	}
	return memory.Normalize(ctx, target)
}

// memoryMigrationPending reports whether target lacks a memory kept with agent-memory, or still
// holds what it kept before it.
func memoryMigrationPending(target string) bool {
	return !memory.Present(target) || memory.LegacyPresent(target) || memoryMentionsPending(target)
}

// migrateMemory gives the application at target its .agent-memory/, takes MEMORY.md and memory/
// into it and removes them, and rewrites the sentences of the application's own files that named
// them. It returns the paths it changed or removed, relative to target, for the caller to commit.
//
// What agent-memory refuses to take stops it with the reason, the old files in place and the memory
// as it was before: an application is never left with half of its memory moved.
func migrateMemory(ctx context.Context, target string, lock *KitLock, isGit bool, out io.Writer) ([]string, error) {
	var changed []string
	legacy := memory.LegacyPresent(target)
	created, err := memory.Init(ctx, target, memoryName(target))
	if err != nil {
		return nil, err
	}
	undo := func() {
		if created {
			_ = os.RemoveAll(filepath.Join(target, filepath.FromSlash(memory.Dir)))
		}
	}
	if created {
		fmt.Fprintf(out, "  %s init created %s/\n", memory.Binary, memory.Dir)
	}
	switch {
	case legacy:
		notes, err := memory.ImportLegacy(ctx, target, time.Now(), created)
		if err != nil {
			undo()
			if !created {
				return nil, fmt.Errorf("%w\n%s/ holds what was taken before that (%d part(s)): undo it with git before running this again", err, memory.Dir, len(notes))
			}
			return nil, err
		}
		for _, note := range notes {
			fmt.Fprintf(out, "  memory: %s\n", note)
		}
	case created:
		if err := seedMemoryConventions(ctx, target); err != nil {
			undo()
			return nil, err
		}
	}
	if created || legacy {
		if err := memory.Normalize(ctx, target); err != nil {
			undo()
			return nil, err
		}
		changed = append(changed, memory.Dir)
	}
	if legacy {
		for _, rel := range []string{memory.LegacyFile, memory.LegacyDir} {
			full := filepath.Join(target, rel)
			if !exists(full) {
				continue
			}
			removed := false
			if isGit {
				// git rm stages the removal; a file git never tracked is removed below.
				if _, err := gitx.RunGit(ctx, target, "rm", "-r", "--quiet", "--", rel); err == nil {
					removed = true
				}
			}
			if !removed || exists(full) {
				if err := os.RemoveAll(full); err != nil {
					return changed, fmt.Errorf("remove %s: %w", rel, err)
				}
				if isGit {
					// Whatever git tracked of it is staged as removed; it says so when it tracked none.
					_, _ = gitx.RunGit(ctx, target, "add", "-A", "--", rel)
				}
			}
			if lock != nil {
				delete(lock.Files, rel)
			}
			changed = append(changed, rel)
		}
		fmt.Fprintf(out, "  memory: %s and %s/ were taken into %s/ and removed; git keeps them as they were\n", memory.LegacyFile, memory.LegacyDir, memory.Dir)
	}
	rewritten, err := rewriteMemoryMentions(target, out)
	if err != nil {
		return changed, err
	}
	return append(changed, rewritten...), nil
}
