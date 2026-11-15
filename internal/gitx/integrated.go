package gitx

import (
	"context"
	"path"
	"regexp"
	"strings"
)

// archiveDirRe is the name of an archived change directory, openspec/changes/archive/<YYYY-MM-DD>-<name>;
// an entry without the date (a README) archives no change.
var archiveDirRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-(.+)$`)

// IntegratedOn returns the changes integrated on ref of the repository at dir: those archived under
// openspec/changes/archive/<date>-<name>/ in ref's tree, and those an integration commit in ref's
// history carries (Repo.CompletedSince). It is the one definition of "integrated on the base": the
// dispatcher skips such a backlog item, and the queue cap does not count it as open. What cannot be
// read is left out.
func IntegratedOn(ctx context.Context, dir, ref string) map[string]bool {
	out := make(map[string]bool)
	if lsOut, err := RunGit(ctx, dir, "ls-tree", "--name-only", ref, "openspec/changes/archive/"); err == nil {
		for _, line := range strings.Split(lsOut, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if m := archiveDirRe.FindStringSubmatch(path.Base(line)); len(m) == 2 {
				out[m[1]] = true
			}
		}
	}
	if r, err := Open(dir); err == nil {
		if completed, err := r.CompletedSince("", ref); err == nil {
			for _, c := range completed {
				if c.ChangeName != "" {
					out[c.ChangeName] = true
				}
			}
		}
	}
	return out
}
