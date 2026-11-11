package gitx

import "context"

// StageGitlinks stages, in the umbrella checkout or worktree at dir, the commit each of the child
// repositories in children is checked out at. A plain "git add <child>" is not enough: when .gitmodules
// says "ignore = all" for the child, git 2.54 and later skip it with exit status 0 unless --force names
// it, and the commit that follows silently keeps the old gitlink.
func StageGitlinks(ctx context.Context, dir string, children ...string) error {
	if len(children) == 0 {
		return nil
	}
	_, err := RunGit(ctx, dir, append([]string{"add", "--force", "--"}, children...)...)
	return err
}
