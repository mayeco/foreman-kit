package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"gopkg.in/yaml.v3"
)

// Repo wraps a go-git repository opened with EnableDotGitCommonDir: true.
type Repo struct {
	dir  string
	repo *git.Repository
}

// Open opens a repository or worktree (.worktrees/*) using go-git with EnableDotGitCommonDir: true.
func Open(dir string) (*Repo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	r, err := git.PlainOpenWithOptions(abs, &git.PlainOpenOptions{
		DetectDotGit:          true,
		EnableDotGitCommonDir: true,
	})
	if err != nil {
		return nil, err
	}
	return &Repo{dir: abs, repo: r}, nil
}

func (r *Repo) Dir() string          { return r.dir }
func (r *Repo) Raw() *git.Repository { return r.repo }

// RevParse resolves a revision string (HEAD, branch, tag, origin/master,HEAD~1) to a full 40-char SHA.
func (r *Repo) RevParse(rev string) (string, error) {
	h, err := r.resolveHash(rev)
	if err != nil {
		return "", err
	}
	return h.String(), nil
}

func (r *Repo) resolveHash(rev string) (plumbing.Hash, error) {
	rev = strings.TrimSpace(rev)
	if rev == "" {
		rev = "HEAD"
	}
	if h, err := r.repo.ResolveRevision(plumbing.Revision(rev)); err == nil && !h.IsZero() {
		return *h, nil
	}
	// Check local branch, remote branch, or tag explicitly
	for _, refName := range []plumbing.ReferenceName{
		plumbing.ReferenceName(rev),
		plumbing.NewBranchReferenceName(rev),
		plumbing.NewRemoteReferenceName("origin", strings.TrimPrefix(rev, "origin/")),
		plumbing.NewTagReferenceName(rev),
	} {
		if ref, err := r.repo.Reference(refName, true); err == nil {
			return ref.Hash(), nil
		}
	}
	if len(rev) == 40 {
		h := plumbing.NewHash(rev)
		if _, err := r.repo.CommitObject(h); err == nil {
			return h, nil
		}
	}
	// Fallback to git rev-parse for complex rev expressions in worktrees
	out, err := RunGit(context.Background(), r.dir, "rev-parse", "--verify", rev)
	if err == nil {
		return plumbing.NewHash(strings.TrimSpace(out)), nil
	}
	return plumbing.ZeroHash, fmt.Errorf("gitx: cannot resolve revision %q in %s: %w", rev, r.dir, err)
}

// MergeBase calculates the best common ancestor SHA between revA and revB using go-git in memory.
func (r *Repo) MergeBase(revA, revB string) (string, error) {
	hashA, err := r.resolveHash(revA)
	if err != nil {
		return "", err
	}
	hashB, err := r.resolveHash(revB)
	if err != nil {
		return "", err
	}
	commitA, err := r.repo.CommitObject(hashA)
	if err != nil {
		return "", err
	}
	commitB, err := r.repo.CommitObject(hashB)
	if err != nil {
		return "", err
	}
	bases, err := commitA.MergeBase(commitB)
	if err != nil || len(bases) == 0 {
		return "", fmt.Errorf("gitx: no merge-base between %s and %s", revA, revB)
	}
	return bases[0].Hash.String(), nil
}

// ReadBlob reads the raw contents of relPath at revision rev directly from the git object store.
func (r *Repo) ReadBlob(rev, relPath string) ([]byte, error) {
	h, err := r.resolveHash(rev)
	if err != nil {
		return nil, err
	}
	commit, err := r.repo.CommitObject(h)
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	f, err := tree.File(filepath.ToSlash(strings.TrimPrefix(relPath, "/")))
	if err != nil {
		return nil, err
	}
	reader, err := f.Reader()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(reader); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// TreeEntryHash returns the hash of a tree entry (including a submodule gitlink) at rev.
func (r *Repo) TreeEntryHash(rev, relPath string) (string, error) {
	h, err := r.resolveHash(rev)
	if err != nil {
		return "", err
	}
	commit, err := r.repo.CommitObject(h)
	if err != nil {
		return "", err
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", err
	}
	entry, err := tree.FindEntry(filepath.ToSlash(strings.TrimPrefix(relPath, "/")))
	if err != nil {
		return "", err
	}
	return entry.Hash.String(), nil
}

// ChangedFiles returns the sorted list of paths changed between baseRev and headRev using go-git tree diff.
func (r *Repo) ChangedFiles(baseRev, headRev string) ([]string, error) {
	baseHash, err := r.resolveHash(baseRev)
	if err != nil {
		return nil, err
	}
	headHash, err := r.resolveHash(headRev)
	if err != nil {
		return nil, err
	}
	baseCommit, err := r.repo.CommitObject(baseHash)
	if err != nil {
		return nil, err
	}
	headCommit, err := r.repo.CommitObject(headHash)
	if err != nil {
		return nil, err
	}
	baseTree, err := baseCommit.Tree()
	if err != nil {
		return nil, err
	}
	headTree, err := headCommit.Tree()
	if err != nil {
		return nil, err
	}
	changes, err := object.DiffTree(baseTree, headTree)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var paths []string
	for _, ch := range changes {
		if ch.From.Name != "" && !seen[ch.From.Name] {
			seen[ch.From.Name] = true
			paths = append(paths, ch.From.Name)
		}
		if ch.To.Name != "" && !seen[ch.To.Name] {
			seen[ch.To.Name] = true
			paths = append(paths, ch.To.Name)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// SubmoduleInfo describes a configured submodule from .gitmodules.
type SubmoduleInfo struct {
	Name   string
	Path   string
	URL    string
	Branch string
}

// Submodules parses .gitmodules using go-git/v5/config.NewModules.
func (r *Repo) Submodules() (map[string]SubmoduleInfo, error) {
	gmPath := filepath.Join(r.dir, ".gitmodules")
	raw, err := os.ReadFile(gmPath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]SubmoduleInfo{}, nil
		}
		return nil, err
	}
	modules := config.NewModules()
	if err := modules.Unmarshal(raw); err != nil {
		return nil, err
	}
	out := make(map[string]SubmoduleInfo, len(modules.Submodules))
	for _, sm := range modules.Submodules {
		p := filepath.ToSlash(strings.TrimSpace(sm.Path))
		if p == "" {
			continue
		}
		out[p] = SubmoduleInfo{
			Name:   sm.Name,
			Path:   p,
			URL:    sm.URL,
			Branch: sm.Branch,
		}
	}
	return out, nil
}

// ChildPaths returns the sorted submodule paths under "repos/".
func (r *Repo) ChildPaths() ([]string, error) {
	subs, err := r.Submodules()
	if err != nil {
		return nil, err
	}
	var out []string
	for p := range subs {
		if strings.HasPrefix(p, "repos/") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// TouchedChildren returns the child repositories (ChildPaths) whose gitlink differs between the
// merge-base of baseRev and headRev and headRev: the children a branch at headRev changes relative to
// baseRev. It reads only the local object database (no fetch, no worktree).
func (r *Repo) TouchedChildren(baseRev, headRev string) ([]string, error) {
	fork, err := r.MergeBase(baseRev, headRev)
	if err != nil {
		return nil, err
	}
	children, err := r.ChildPaths()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, cp := range children {
		h1, err1 := r.TreeEntryHash(fork, cp)
		h2, err2 := r.TreeEntryHash(headRev, cp)
		if err1 == nil && err2 == nil && h1 != h2 {
			out = append(out, cp)
		}
	}
	return out, nil
}

// ChildBaseBranch resolves the base branch of a child repo: repo.config.yaml base_branch -> .gitmodules branch -> "main".
func (r *Repo) ChildBaseBranch(childPath string) string {
	cfgFile := filepath.Join(r.dir, filepath.FromSlash(childPath), "repo.config.yaml")
	if data, err := os.ReadFile(cfgFile); err == nil {
		var m map[string]any
		if yaml.Unmarshal(data, &m) == nil {
			if b, ok := m["base_branch"].(string); ok && b != "" && !strings.Contains(b, "{{") {
				return b
			}
		}
	}
	if subs, err := r.Submodules(); err == nil {
		if sm, ok := subs[filepath.ToSlash(childPath)]; ok && sm.Branch != "" {
			return sm.Branch
		}
	}
	return "main"
}

// IntegratedChange represents a commit that integrated an OpenSpec change.
type IntegratedChange struct {
	SHA        string `json:"sha"`
	ChangeName string `json:"change_name"`
	Subject    string `json:"subject"`
	Committed  string `json:"committed_at"`
}

// CompletedSince lists, oldest first, every commit reachable from headRev but not from baseRev
// (git log --reverse --topo-order baseRev..headRev; all commits when baseRev == "") that carries an
// "OpenSpec-Change: <name>" trailer. History with merge commits (a merged kit-upgrade branch, for
// example) is handled like git does: an ancestor of baseRev is excluded whichever parent leads to it.
func (r *Repo) CompletedSince(baseRev, headRev string) ([]IntegratedChange, error) {
	headHash, err := r.resolveHash(headRev)
	if err != nil {
		return nil, err
	}
	head, err := r.repo.CommitObject(headHash)
	if err != nil {
		return nil, err
	}

	// excluded holds every commit reachable from baseRev, whichever parent path leads to it.
	excluded := make(map[plumbing.Hash]bool)
	if baseRev != "" {
		if baseHash, err := r.resolveHash(baseRev); err == nil && !baseHash.IsZero() {
			if base, err := r.repo.CommitObject(baseHash); err == nil {
				_ = object.NewCommitPreorderIter(base, nil, nil).ForEach(func(c *object.Commit) error {
					excluded[c.Hash] = true
					return nil
				})
			}
		}
	}
	if excluded[head.Hash] {
		return nil, nil
	}

	// Iterative post-order DFS: each commit is emitted after all of its non-excluded parents.
	type frame struct {
		c    *object.Commit
		next int
	}
	visited := map[plumbing.Hash]bool{head.Hash: true}
	stack := []frame{{c: head}}
	var list []IntegratedChange
	for len(stack) > 0 {
		top := &stack[len(stack)-1]
		if top.next < len(top.c.ParentHashes) {
			ph := top.c.ParentHashes[top.next]
			top.next++
			if visited[ph] || excluded[ph] {
				continue
			}
			visited[ph] = true
			if p, err := r.repo.CommitObject(ph); err == nil {
				stack = append(stack, frame{c: p})
			}
			continue
		}
		c := top.c
		stack = stack[:len(stack)-1]
		if chg := extractOpenSpecTrailer(c.Message); chg != "" {
			list = append(list, IntegratedChange{
				SHA:        c.Hash.String(),
				ChangeName: chg,
				Subject:    strings.TrimSpace(strings.SplitN(c.Message, "\n", 2)[0]),
				Committed:  c.Committer.When.UTC().Format(time.RFC3339),
			})
		}
	}
	return list, nil
}

// HasCommitWithTrailer checks whether any commit reachable from rev carries "OpenSpec-Change: <changeName>".
func (r *Repo) HasCommitWithTrailer(rev, changeName string) (bool, error) {
	items, err := r.CompletedSince("", rev)
	if err != nil {
		return false, err
	}
	for _, it := range items {
		if it.ChangeName == changeName {
			return true, nil
		}
	}
	return false, nil
}

// SaveUnpublishedCommit creates a commit directly in the Git object store (without touching .git/index)
// overlaying the given relative file paths from disk onto HEAD's tree, and stores a reference under refName
// (e.g. "refs/openspec/unpublished/<worktree>-<stamp>").
func (r *Repo) SaveUnpublishedCommit(refName, message string, relPaths []string) (string, error) {
	storer := r.repo.Storer
	var parentHashes []plumbing.Hash
	baseEntries := make(map[string]object.TreeEntry)

	if headHash, err := r.resolveHash("HEAD"); err == nil && !headHash.IsZero() {
		parentHashes = append(parentHashes, headHash)
		if headCommit, err := r.repo.CommitObject(headHash); err == nil {
			if headTree, err := headCommit.Tree(); err == nil {
				_ = flattenTreeEntries(headTree, "", baseEntries)
			}
		}
	}

	for _, rel := range relPaths {
		rel = filepath.ToSlash(strings.TrimPrefix(rel, "/"))
		if rel == "" {
			continue
		}
		fullPath := filepath.Join(r.dir, filepath.FromSlash(rel))
		data, err := os.ReadFile(fullPath)
		if err != nil {
			if os.IsNotExist(err) {
				delete(baseEntries, rel)
				continue
			}
			return "", err
		}
		obj := storer.NewEncodedObject()
		obj.SetType(plumbing.BlobObject)
		obj.SetSize(int64(len(data)))
		w, err := obj.Writer()
		if err != nil {
			return "", err
		}
		if _, err := w.Write(data); err != nil {
			_ = w.Close()
			return "", err
		}
		_ = w.Close()
		blobHash, err := storer.SetEncodedObject(obj)
		if err != nil {
			return "", err
		}
		baseEntries[rel] = object.TreeEntry{
			Name: filepath.Base(rel),
			Mode: filemode.Regular,
			Hash: blobHash,
		}
	}

	rootTreeHash, err := buildNestedTree(storer, baseEntries)
	if err != nil {
		return "", err
	}

	now := time.Now().UTC()
	sig := object.Signature{
		Name:  "foreman-kit",
		Email: "foreman-kit@localhost",
		When:  now,
	}
	commit := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      message,
		TreeHash:     rootTreeHash,
		ParentHashes: parentHashes,
	}
	commitObj := storer.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return "", err
	}
	commitHash, err := storer.SetEncodedObject(commitObj)
	if err != nil {
		return "", err
	}

	if !strings.HasPrefix(refName, "refs/") {
		refName = "refs/openspec/unpublished/" + refName
	}
	ref := plumbing.NewHashReference(plumbing.ReferenceName(refName), commitHash)
	if err := storer.SetReference(ref); err != nil {
		return "", err
	}
	return commitHash.String(), nil
}

func flattenTreeEntries(t *object.Tree, prefix string, out map[string]object.TreeEntry) error {
	for _, e := range t.Entries {
		rel := e.Name
		if prefix != "" {
			rel = prefix + "/" + e.Name
		}
		if e.Mode == filemode.Dir {
			sub, err := t.Tree(e.Name)
			if err != nil {
				return err
			}
			if err := flattenTreeEntries(sub, rel, out); err != nil {
				return err
			}
			continue
		}
		out[rel] = e
	}
	return nil
}

func buildNestedTree(s storer.EncodedObjectStorer, flat map[string]object.TreeEntry) (plumbing.Hash, error) {
	type dirNode struct {
		files   map[string]object.TreeEntry
		subdirs map[string]*dirNode
	}
	newNode := func() *dirNode {
		return &dirNode{
			files:   make(map[string]object.TreeEntry),
			subdirs: make(map[string]*dirNode),
		}
	}
	root := newNode()
	for rel, entry := range flat {
		parts := strings.Split(rel, "/")
		cur := root
		for i := 0; i < len(parts)-1; i++ {
			p := parts[i]
			next, ok := cur.subdirs[p]
			if !ok {
				next = newNode()
				cur.subdirs[p] = next
			}
			cur = next
		}
		leaf := parts[len(parts)-1]
		entry.Name = leaf
		cur.files[leaf] = entry
	}

	var writeNode func(n *dirNode) (plumbing.Hash, error)
	writeNode = func(n *dirNode) (plumbing.Hash, error) {
		var entries []object.TreeEntry
		for _, f := range n.files {
			entries = append(entries, f)
		}
		for name, sub := range n.subdirs {
			subHash, err := writeNode(sub)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			entries = append(entries, object.TreeEntry{
				Name: name,
				Mode: filemode.Dir,
				Hash: subHash,
			})
		}
		sort.Slice(entries, func(i, j int) bool {
			ni := entries[i].Name
			if entries[i].Mode == filemode.Dir {
				ni += "/"
			}
			nj := entries[j].Name
			if entries[j].Mode == filemode.Dir {
				nj += "/"
			}
			return ni < nj
		})
		tree := &object.Tree{Entries: entries}
		obj := s.NewEncodedObject()
		if err := tree.Encode(obj); err != nil {
			return plumbing.ZeroHash, err
		}
		return s.SetEncodedObject(obj)
	}
	return writeNode(root)
}

func extractOpenSpecTrailer(msg string) string {
	for _, line := range strings.Split(msg, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "OpenSpec-Change:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "OpenSpec-Change:"))
		}
	}
	return ""
}

// gitWaitDelay is how long RunGit gives git to exit after SIGTERM before SIGKILL (time to delete
// index.lock and its other lock files and to stop its transport and hook children), and how long it waits
// after git exits for descendants that inherited git's stdout/stderr, such as a hook that left a process
// running.
const gitWaitDelay = 15 * time.Second

// repoLocalEnv names the variables that tie a git process to one repository, index or object store: the
// ones git itself clears before it runs a command in another repository (git rev-parse
// --local-env-vars, without GIT_CONFIG_PARAMETERS and GIT_CONFIG_COUNT, which carry "git -c" settings).
var repoLocalEnv = map[string]bool{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
	"GIT_CONFIG":                       true,
	"GIT_DIR":                          true,
	"GIT_GRAFT_FILE":                   true,
	"GIT_IMPLICIT_WORK_TREE":           true,
	"GIT_INDEX_FILE":                   true,
	"GIT_NO_REPLACE_OBJECTS":           true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_PREFIX":                       true,
	"GIT_REPLACE_REF_BASE":             true,
	"GIT_SHALLOW_FILE":                 true,
	"GIT_WORK_TREE":                    true,
}

// DropRepoLocalEnv removes the repoLocalEnv variables from this process's environment, so that no command
// it starts inherits them either: an agent CLI, a gate, a detached daemon or cycle. RunGit already leaves
// them out for the git it runs itself, but a process started from a git hook, a "!" alias or
// "git rebase --exec" handed GIT_DIR, GIT_INDEX_FILE and GIT_WORK_TREE on to those commands, and every git
// an agent or a gate then ran in its worktree acted on the hook's repository and index instead.
func DropRepoLocalEnv() {
	for name := range repoLocalEnv {
		_ = os.Unsetenv(name)
	}
}

// RunGit runs a git command in dir with SSH_AUTH_SOCK discovered when needed.
func RunGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// The repository is the one dir is in, never one the environment names: a process started from a
	// git hook, a "!" alias or "git rebase --exec" inherits GIT_DIR, GIT_INDEX_FILE and GIT_WORK_TREE.
	environ := os.Environ()
	env := make([]string, 0, len(environ)+3)
	for _, kv := range environ {
		if name, _, _ := strings.Cut(kv, "="); !repoLocalEnv[name] {
			env = append(env, kv)
		}
	}
	if os.Getenv("FOREMANKIT_SANITIZE_GIT_ENV") != "1" && os.Getenv("SSH_AUTH_SOCK") == "" {
		if sock := DiscoverSSHAuthSock(""); sock != "" {
			env = append(env, "SSH_AUTH_SOCK="+sock)
		}
	}
	// Callers match git's messages (RebaseBranch, conflict file extraction): LANGUAGE=C keeps them
	// untranslated without touching LC_CTYPE, which governs how paths are encoded.
	env = append(env, "LANGUAGE=C")
	if os.Getenv("GIT_TERMINAL_PROMPT") == "" {
		// Never block an unattended run on a credential prompt; GIT_TERMINAL_PROMPT=1 opts back in.
		env = append(env, "GIT_TERMINAL_PROMPT=0")
	}
	cmd.Env = env
	// On cancellation send SIGTERM: git handles it by deleting its lock files and stopping its children.
	// The os/exec default, SIGKILL, leaves .git/index.lock and ref locks behind.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = gitWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		// git succeeded; only a descendant kept the output pipes open past gitWaitDelay.
		err = nil
	}
	if err != nil {
		combined := stdout.String()
		if stderr.Len() > 0 {
			if combined != "" && !strings.HasSuffix(combined, "\n") {
				combined += "\n"
			}
			combined += stderr.String()
		}
		return combined, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(combined))
	}
	return stdout.String(), nil
}
