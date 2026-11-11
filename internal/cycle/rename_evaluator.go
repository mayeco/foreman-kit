package cycle

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/gitx"
)

// An application installed while the evaluator was named after its provider keeps two things under
// the old name: the file with its gates and the placeholder of its prompts. `foreman-kit update`
// renames them; nothing reads the old names afterwards. The value that said the provider approves
// proposals is moved with the other keys of that kit (config.MigrateConfig).

const (
	legacyHintPlaceholder = "{{jev_hint}}"
	hintPlaceholder       = "{{evaluator_hint}}"
)

// promptFiles lists the prompts of an application, relative to its root.
var promptFiles = []string{".foreman-kit/prompts/plan.md", ".foreman-kit/prompts/build.md", ".foreman-kit/prompts/cycle.md"}

// evaluatorRenamePending reports whether target still holds a name from before the evaluator was
// renamed.
func evaluatorRenamePending(target string) bool {
	if fileExists(filepath.Join(target, filepath.FromSlash(evaluator.LegacyPolicyFile))) {
		return true
	}
	for _, rel := range promptFiles {
		if raw, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel))); err == nil && strings.Contains(string(raw), legacyHintPlaceholder) {
			return true
		}
	}
	return false
}

// renameEvaluator rewrites what target still holds under the provider's name: it renames
// config/jev_policy.yaml to .foreman-kit/evaluator.yaml (moving its entry in lock with it) and renames the
// {{jev_hint}} placeholder of the prompts. It returns the files it changed or removed, relative to target, for the caller to commit.
func renameEvaluator(ctx context.Context, target string, lock *KitLock, isGit bool, out io.Writer) ([]string, error) {
	var changed []string

	oldRel, newRel := evaluator.LegacyPolicyFile, evaluator.PolicyFile
	oldFull := filepath.Join(target, filepath.FromSlash(oldRel))
	newFull := filepath.Join(target, filepath.FromSlash(newRel))
	if fileExists(oldFull) {
		if fileExists(newFull) {
			fmt.Fprintf(out, "  %s is no longer read: %s exists too. Move what you need into it, then delete %s\n", oldRel, newRel, oldRel)
		} else {
			moved := false
			if isGit {
				// git mv keeps the history of the file and stages the removal of the old name.
				if _, err := gitx.RunGit(ctx, target, "mv", "--", oldRel, newRel); err == nil {
					moved = true
				}
			}
			if !moved {
				if err := os.Rename(oldFull, newFull); err != nil {
					return changed, fmt.Errorf("rename %s to %s: %w", oldRel, newRel, err)
				}
			}
			if lock != nil {
				if entry, ok := lock.Files[oldRel]; ok {
					lock.Files[newRel] = entry
					delete(lock.Files, oldRel)
				}
			}
			changed = append(changed, oldRel, newRel)
			fmt.Fprintf(out, "  renamed %s to %s\n", oldRel, newRel)
		}
	}

	for _, rel := range promptFiles {
		full := filepath.Join(target, filepath.FromSlash(rel))
		raw, err := os.ReadFile(full)
		if err != nil || !strings.Contains(string(raw), legacyHintPlaceholder) {
			continue
		}
		updated := strings.ReplaceAll(string(raw), legacyHintPlaceholder, hintPlaceholder)
		if err := os.WriteFile(full, []byte(updated), 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, rel)
		fmt.Fprintf(out, "  %s: %s -> %s\n", rel, legacyHintPlaceholder, hintPlaceholder)
	}
	return changed, nil
}
