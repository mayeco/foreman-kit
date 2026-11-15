package gitx

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// owcIsolate keeps the machine's git configuration out of the fixtures.
func owcIsolate(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// owcGit runs system git in dir and returns its standard output, trimmed.
func owcGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// owcCommit writes each name=content pair under dir and commits it.
func owcCommit(t *testing.T, dir, message string, files ...string) {
	t.Helper()
	for i := 0; i+1 < len(files); i += 2 {
		path := filepath.Join(dir, files[i])
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(files[i+1]), 0o644); err != nil {
			t.Fatal(err)
		}
		owcGit(t, dir, "add", files[i])
	}
	owcGit(t, dir, "commit", "--quiet", "-m", message)
}

// owcInit creates a repository on branch main with one commit.
func owcInit(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	owcGit(t, dir, "init", "--quiet")
	owcGit(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")
	owcCommit(t, dir, "base", "a.txt", "a\n", "b.txt", "b\n", "docs/readme.md", "readme\n")
}

// owcHistory creates a repository whose branches main and side share the base commit. Each
// branch modifies one file and adds another, and none renames a file. main is checked out.
func owcHistory(t *testing.T, dir string) {
	t.Helper()
	owcInit(t, dir)
	owcGit(t, dir, "checkout", "--quiet", "-b", "side")
	owcCommit(t, dir, "side work", "b.txt", "b side\n", "d.txt", "d\n")
	owcGit(t, dir, "checkout", "--quiet", "main")
	owcCommit(t, dir, "main work", "a.txt", "a main\n", "docs/c.txt", "c\n")
}

// owcAppend appends text to the file at path.
func owcAppend(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// owcSnapshot reads each configuration file.
func owcSnapshot(t *testing.T, paths []string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[p] = b
	}
	return out
}

// owcOpenAndCompare opens dir with Open and checks that HEAD, the merge base of revA and revB
// and the files changed between them equal what system git prints in dir. It also checks that
// the repository keeps the plain storage, that its configuration still holds
// extensions.worktreeConfig, and that none of the configuration files changed on disk.
func owcOpenAndCompare(t *testing.T, dir, revA, revB string, configs ...string) *Repo {
	t.Helper()
	before := owcSnapshot(t, configs)

	r, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	head, err := r.RevParse("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if want := owcGit(t, dir, "rev-parse", "HEAD"); head != want {
		t.Fatalf("RevParse(HEAD) = %s, git rev-parse HEAD = %s", head, want)
	}
	base, err := r.MergeBase(revA, revB)
	if err != nil {
		t.Fatal(err)
	}
	if want := owcGit(t, dir, "merge-base", revA, revB); base != want {
		t.Fatalf("MergeBase(%s, %s) = %s, git merge-base = %s", revA, revB, base, want)
	}
	changed, err := r.ChangedFiles(revA, revB)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Split(owcGit(t, dir, "diff", "--no-renames", "--name-only", revA, revB), "\n")
	sort.Strings(want)
	if !slices.Equal(changed, want) {
		t.Fatalf("ChangedFiles(%s, %s) = %v, git diff --name-only = %v", revA, revB, changed, want)
	}

	if _, ok := r.Raw().Storer.(*filesystem.Storage); !ok {
		t.Fatalf("Raw().Storer is %T, want *filesystem.Storage", r.Raw().Storer)
	}
	cfg, err := r.Raw().Config()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Raw.HasSection("extensions") || !cfg.Raw.Section("extensions").HasOption("worktreeConfig") {
		t.Fatalf("Raw().Config() lost extensions.worktreeConfig: %#v", cfg.Raw.Sections)
	}

	for p, b := range owcSnapshot(t, configs) {
		if !bytes.Equal(before[p], b) {
			t.Fatalf("%s changed while opening and reading:\nbefore:\n%s\nafter:\n%s", p, before[p], b)
		}
	}
	return r
}

func TestOpenFindsTheRepositoryFromASubdirectoryOrAFile(t *testing.T) {
	owcIsolate(t)
	repo := filepath.Join(t.TempDir(), "repo")
	owcHistory(t, repo)
	want := owcGit(t, repo, "rev-parse", "HEAD")

	for _, dir := range []string{
		repo,
		filepath.Join(repo, "docs"),
		filepath.Join(repo, "docs", "readme.md"),
	} {
		r, err := Open(dir)
		if err != nil {
			t.Fatalf("Open(%s): %v", dir, err)
		}
		if got, err := r.RevParse("HEAD"); err != nil || got != want {
			t.Fatalf("Open(%s).RevParse(HEAD) = %s, %v; want %s", dir, got, err, want)
		}
	}
}

func TestOpenRefusesADirectoryOutsideAnyRepository(t *testing.T) {
	owcIsolate(t)
	dir := filepath.Join(t.TempDir(), "outside", "deeper")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	probe := exec.Command("git", "-C", dir, "rev-parse", "--git-dir")
	if probe.Run() == nil {
		t.Skipf("%s is inside a git repository on this machine", dir)
	}
	_, err := Open(dir)
	if !errors.Is(err, git.ErrRepositoryNotExists) {
		t.Fatalf("Open(%s) = %v, want %v", dir, err, git.ErrRepositoryNotExists)
	}
}

func TestOpenWorktreeConfigUnderEachFormatVersion(t *testing.T) {
	for _, version := range []string{"0", "1"} {
		t.Run("version "+version, func(t *testing.T) {
			owcIsolate(t)
			repo := filepath.Join(t.TempDir(), "repo")
			owcHistory(t, repo)
			owcGit(t, repo, "config", "core.repositoryformatversion", version)
			owcGit(t, repo, "config", "extensions.worktreeConfig", "true")

			owcOpenAndCompare(t, repo, "main", "side", filepath.Join(repo, ".git", "config"))
		})
	}
}

func TestOpenWorktreeConfigInAnyLetterCase(t *testing.T) {
	owcIsolate(t)
	repo := filepath.Join(t.TempDir(), "repo")
	owcHistory(t, repo)
	config := filepath.Join(repo, ".git", "config")
	owcAppend(t, config, "[extensions]\n\tWORKTREECONFIG = true\n")

	owcOpenAndCompare(t, repo, "main", "side", config)
}

func TestOpenWorktreeConfigInALinkedWorktree(t *testing.T) {
	owcIsolate(t)
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	owcInit(t, repo)
	owcGit(t, repo, "config", "extensions.worktreeConfig", "true")

	wt := filepath.Join(base, "linked")
	owcGit(t, repo, "worktree", "add", "-b", "side", wt)
	owcCommit(t, wt, "side work", "b.txt", "b side\n", "d.txt", "d\n")
	owcCommit(t, repo, "main work", "a.txt", "a main\n", "docs/c.txt", "c\n")

	gitDir := owcGit(t, wt, "rev-parse", "--git-dir")
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(wt, gitDir)
	}
	worktreeConfig := filepath.Join(gitDir, "config.worktree")
	if err := os.WriteFile(worktreeConfig, []byte("[core]\n\tsparseCheckout = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configs := []string{filepath.Join(repo, ".git", "config"), worktreeConfig}

	r := owcOpenAndCompare(t, wt, "main", "HEAD", configs...)
	head, err := r.RevParse("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if side, mainTip := owcGit(t, repo, "rev-parse", "side"), owcGit(t, repo, "rev-parse", "main"); head != side || head == mainTip {
		t.Fatalf("linked worktree HEAD = %s, want the side commit %s and not the main checkout's %s", head, side, mainTip)
	}

	// The main checkout opens too, and opening both leaves both files as they were.
	owcOpenAndCompare(t, repo, "side", "HEAD", configs...)
}

func TestOpenWorktreeConfigInASubmoduleCheckout(t *testing.T) {
	owcIsolate(t)
	base := t.TempDir()
	child := filepath.Join(base, "child")
	owcHistory(t, child)

	super := filepath.Join(base, "super")
	owcInit(t, super)
	owcGit(t, super, "-c", "protocol.file.allow=always", "submodule", "--quiet", "add", child, "sub")
	sub := filepath.Join(super, "sub")
	owcGit(t, sub, "config", "extensions.worktreeConfig", "true")

	moduleConfig := filepath.Join(super, ".git", "modules", "sub", "config")
	b, err := os.ReadFile(moduleConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(string(b)), "worktreeconfig = true") {
		t.Fatalf("git config in the submodule did not write %s:\n%s", moduleConfig, b)
	}

	owcOpenAndCompare(t, sub, "origin/main", "origin/side", moduleConfig, filepath.Join(super, ".git", "config"))
}

func TestOpenWorktreeConfigDoesNotExcuseAnotherExtension(t *testing.T) {
	for name, extensions := range map[string]string{
		"alone":                 "[extensions]\n\tfoo = bar\n",
		"beside worktreeConfig": "[extensions]\n\tworktreeConfig = true\n\tfoo = bar\n",
	} {
		t.Run(name, func(t *testing.T) {
			owcIsolate(t)
			repo := filepath.Join(t.TempDir(), "repo")
			owcHistory(t, repo)
			owcGit(t, repo, "config", "core.repositoryformatversion", "1")
			owcAppend(t, filepath.Join(repo, ".git", "config"), extensions)

			r, err := Open(repo)
			if err == nil {
				t.Fatalf("Open(%s) opened a repository that needs extension foo: %v", repo, r)
			}
			if !errors.Is(err, git.ErrUnknownExtension) {
				t.Fatalf("Open(%s) = %v, want %v", repo, err, git.ErrUnknownExtension)
			}
			if !strings.Contains(err.Error(), "foo") {
				t.Fatalf("Open(%s) = %q, want the error to name extension foo", repo, err)
			}
		})
	}
}
