package cycle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/store"
)

// The kit was called openspec-kit before it was called Foreman. An application installed then keeps
// the kit's files in config/ and its lock in .openspec-kit.lock, its guides and prompts tell agents
// to run openspec-kit and to read the openspec-kit-* skills, and its variables are OPENSPEC_*.
// foreman-kit update moves the files into .foreman-kit/ (git mv, so their history follows) and rewrites
// those names where the application's own files use them; the rest of each file stays as written.

// ForemanDir is the folder of the kit in an application.
const ForemanDir = ".foreman-kit"

// formerKitFiles are the files the kit kept in config/ before, by name, and the folders.
var formerKitFiles = []string{
	"pipeline.yaml", "agents.yaml", "cycle.yaml", "daemon.yaml", "visual_qa.yaml", "exploration.yaml",
	"evaluator.yaml", "jev_policy.yaml", "agent_backlog.json", "agent_history.json", "agent_proposals.json",
	"prompts", "templates",
}

var formerRotatedHistory = regexp.MustCompile(`^agent_history\.\d+\.json(\.new)?$`)

// formerNameRules rewrite, in the text of an application's own files, what names the kit as it was
// called before: the same rules the kit's repository was renamed with.
var formerNameRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`\.openspec-kit\.lock`), ".foreman-kit/kit.lock"},
	{regexp.MustCompile(`~/\.openspec-kit`), "~/.foreman-kit"},
	{regexp.MustCompile(`OPENSPEC_KIT_HOME`), "FOREMANKIT_HOME"},
	{regexp.MustCompile(`openspec-kit-(darwin|linux)`), "foreman-kit-$1"},
	{regexp.MustCompile(`OpenSpec orchestration kit`), "Foreman"},
	{regexp.MustCompile(`openspec-kit`), "foreman-kit"},
	{regexp.MustCompile(`config/((pipeline|agents|cycle|daemon|visual_qa|exploration|evaluator)\.yaml|agent_backlog\.json|agent_history|agent_proposals\.json|prompts|templates|\*\.yaml|\*\.new|<file>)`), ".foreman-kit/$1"},
	{regexp.MustCompile(`\bOPENSPEC_([A-Z])`), "FOREMANKIT_$1"},
}

// RewriteFormerNames returns text with the names of the kit as it was called before replaced by its
// names of today.
func RewriteFormerNames(text string) string {
	for _, r := range formerNameRules {
		text = r.re.ReplaceAllString(text, r.repl)
	}
	return text
}

// foremanLayoutPending reports whether target still holds the kit as it was laid out before it was
// called Foreman.
func foremanLayoutPending(target string) bool {
	if fileExists(filepath.Join(target, store.LegacyLockFile)) {
		return true
	}
	for _, name := range formerKitFiles {
		if exists(filepath.Join(target, "config", name)) {
			return true
		}
	}
	return false
}

// moveToForeman moves the kit's files of target from config/ and .openspec-kit.lock into .foreman-kit/,
// renames in the lock the paths it records, and rewrites the former names in the application's own
// guides, prompts and settings. A file that exists in both places stays where it is, and is named.
// It returns the paths it changed, relative to target, for the caller to commit.
func moveToForeman(ctx context.Context, target string, isGit bool, out io.Writer) ([]string, error) {
	var changed []string
	if err := os.MkdirAll(filepath.Join(target, ForemanDir), 0o755); err != nil {
		return nil, err
	}
	move := func(from, to string) error {
		src, dst := filepath.Join(target, filepath.FromSlash(from)), filepath.Join(target, filepath.FromSlash(to))
		if !exists(src) {
			return nil
		}
		if exists(dst) {
			fmt.Fprintf(out, "  %s stays: %s exists already. Merge what you need into it, then delete %s\n", from, to, from)
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		moved := false
		if isGit {
			if _, err := gitx.RunGit(ctx, target, "mv", "-k", "--", from, to); err == nil && !exists(src) {
				moved = true
			}
		}
		if !moved {
			if err := os.Rename(src, dst); err != nil {
				return fmt.Errorf("move %s to %s: %w", from, to, err)
			}
		}
		changed = append(changed, from, to)
		return nil
	}

	if err := move(store.LegacyLockFile, KitLockFile); err != nil {
		return changed, err
	}
	names := append([]string{}, formerKitFiles...)
	if entries, err := os.ReadDir(filepath.Join(target, "config")); err == nil {
		for _, e := range entries {
			n := e.Name()
			if formerRotatedHistory.MatchString(n) {
				names = append(names, n)
				continue
			}
			for _, k := range formerKitFiles {
				if n == k+".new" {
					names = append(names, n)
				}
			}
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if err := move("config/"+n, ForemanDir+"/"+n); err != nil {
			return changed, err
		}
	}
	// config/ goes once nothing of the application is left in it.
	if entries, err := os.ReadDir(filepath.Join(target, "config")); err == nil && len(entries) == 0 {
		_ = os.Remove(filepath.Join(target, "config"))
	}
	if len(changed) > 0 {
		fmt.Fprintf(out, "  moved the kit's files from config/ and %s into %s/\n", store.LegacyLockFile, ForemanDir)
	}

	// The lock records the kit's files by path.
	if raw, err := os.ReadFile(filepath.Join(target, KitLockFile)); err == nil {
		var lock KitLock
		if json.Unmarshal(raw, &lock) == nil && lock.Files != nil {
			renamed := make(map[string]LockFileEntry, len(lock.Files))
			for rel, entry := range lock.Files {
				if strings.HasPrefix(rel, "config/") {
					rel = ForemanDir + "/" + strings.TrimPrefix(rel, "config/")
				}
				renamed[rel] = entry
			}
			lock.Files = renamed
			if b, err := json.MarshalIndent(lock, "", "  "); err == nil && string(append(b, '\n')) != string(raw) {
				if err := os.WriteFile(filepath.Join(target, KitLockFile), append(b, '\n'), 0o644); err != nil {
					return changed, err
				}
			}
		}
	}

	// The names in the application's own files.
	files := []string{"AGENTS.md", "CLAUDE.md", "README.md", "ROADMAP.md", ".gitignore"}
	for _, pattern := range []string{ForemanDir + "/*.yaml", ForemanDir + "/prompts/*.md", ForemanDir + "/templates/*"} {
		matches, _ := filepath.Glob(filepath.Join(target, filepath.FromSlash(pattern)))
		for _, m := range matches {
			if rel, err := filepath.Rel(target, m); err == nil {
				files = append(files, filepath.ToSlash(rel))
			}
		}
	}
	var rewritten []string
	for _, rel := range files {
		full := filepath.Join(target, filepath.FromSlash(rel))
		if st, err := os.Stat(full); err != nil || st.IsDir() {
			continue
		}
		raw, err := os.ReadFile(full)
		if err != nil {
			return changed, err
		}
		text := RewriteFormerNames(string(raw))
		if text == string(raw) {
			continue
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			return changed, err
		}
		rewritten = append(rewritten, rel)
		changed = append(changed, rel)
	}
	if len(rewritten) > 0 {
		fmt.Fprintf(out, "  %s: they name the kit as Foreman (foreman-kit, .foreman-kit/, foreman-kit-* skills, FOREMANKIT_*)\n", strings.Join(rewritten, ", "))
	}
	return changed, nil
}
