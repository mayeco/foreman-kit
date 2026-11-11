package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// commandLimit bounds one call of agent-memory. It answers in a fraction of a second; a call that
// takes this long is stuck, and what waits for it (an install, the record of a cycle) is not left
// waiting with it.
const commandLimit = 2 * time.Minute

// run runs agent-memory with args in dir, gives it stdin when there is one, and returns what it
// wrote to its standard output. An error carries what the program said.
func run(ctx context.Context, dir, stdin string, args ...string) (string, error) {
	if err := Require(); err != nil {
		return "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, commandLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, Binary, args...)
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		said := strings.TrimSpace(stderr.String())
		if said == "" {
			said = strings.TrimSpace(stdout.String())
		}
		if said == "" {
			said = err.Error()
		}
		return stdout.String(), &commandError{args: args, said: said, cause: err}
	}
	return stdout.String(), nil
}

// commandError is a call of agent-memory that failed, with what the program said.
type commandError struct {
	args  []string
	said  string
	cause error
}

func (e *commandError) Error() string {
	return fmt.Sprintf("%s %s: %s", Binary, e.args[0], e.said)
}

func (e *commandError) Unwrap() error { return e.cause }

// Version returns the version of the agent-memory on PATH ("v0.6.1"), or "" when it cannot be run.
func Version(ctx context.Context) string {
	out, err := run(ctx, "", "", "version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
}

// Init gives the application at root its memory, named name, with `agent-memory init`. An
// application that already has one keeps it as it is. It reports whether it created it.
func Init(ctx context.Context, root, name string) (created bool, err error) {
	if Present(root) {
		return false, nil
	}
	if err := Require(); err != nil {
		return false, err
	}
	if _, err := run(ctx, root, "", "init", "--root", root, "--name", name); err != nil {
		return false, err
	}
	if !Present(root) {
		return false, fmt.Errorf("%s init ran in %s and left no %s", Binary, root, manifestFile)
	}
	return true, nil
}

// Proposal is one change to the memory, as `agent-memory propose` takes it.
type Proposal struct {
	// Intent is what the change is: add_pitfall, update_conventions, record_decision or
	// refresh_module.
	Intent string
	// Op is the operation: append_to_section, replace_section_content, append_section or
	// create_file.
	Op string
	// Path is the file, relative to .agent-memory/.
	Path string
	// SectionID is the section an append_to_section adds to, or the one a replace_section_content
	// gives a new body.
	SectionID string
	// ParentSectionID is the section an append_section adds a section under.
	ParentSectionID string
	// Heading and HeadingLevel are those of the section an append_section adds.
	Heading      string
	HeadingLevel int
	// Content is the text. For a new section or file it starts with the heading and its
	// "<!-- @id: ... -->" line.
	Content string
	// Source says where the text comes from, as type:ref ("file:AGENTS.md").
	Source string
	// Confidence is confirmed, inferred or user-provided.
	Confidence string
}

// proposeResult is what `agent-memory propose --json` answers, as far as it is read here.
type proposeResult struct {
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Applied *struct {
		Status string `json:"status"`
	} `json:"applied"`
	Violations []struct {
		Field   string `json:"Field"`
		Message string `json:"Message"`
	} `json:"violations"`
	ProvenanceViolations []string `json:"provenance_violations"`
}

// Propose writes one change to the memory of the application at root through `agent-memory propose
// --apply`: agent-memory checks it (its form, where it comes from, secrets in it) and writes it, or
// says why it does not. A change it refuses is an error that carries its reasons; nothing was
// written then.
func Propose(ctx context.Context, root string, p Proposal) error {
	args := []string{"propose", "--root", root, "--json", "--apply",
		"--intent", p.Intent, "--op", p.Op, "--path", p.Path, "--content-file", "-"}
	if p.SectionID != "" {
		args = append(args, "--section-id", p.SectionID)
	}
	if p.ParentSectionID != "" {
		args = append(args, "--parent-section-id", p.ParentSectionID)
	}
	if p.Heading != "" {
		args = append(args, "--heading", p.Heading, "--heading-level", strconv.Itoa(p.HeadingLevel))
	}
	if p.Source != "" {
		args = append(args, "--source", p.Source)
	}
	if p.Confidence != "" {
		args = append(args, "--confidence", p.Confidence)
	}
	out, runErr := run(ctx, root, p.Content, args...)
	var res proposeResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		if runErr != nil {
			return runErr
		}
		return fmt.Errorf("%s propose answered something that is not JSON for %s: %.200s", Binary, p.Path, out)
	}
	switch {
	case res.Status == "applied", res.Applied != nil && res.Applied.Status == "applied":
		return nil
	}
	why := []string{}
	if res.Message != "" {
		why = append(why, res.Message)
	}
	for _, v := range res.Violations {
		why = append(why, v.Message)
	}
	why = append(why, res.ProvenanceViolations...)
	if len(why) == 0 && runErr != nil {
		why = append(why, runErr.Error())
	}
	state := res.Status
	if res.Reason != "" {
		state += ", " + res.Reason
	}
	return fmt.Errorf("%s did not write %s/%s (%s): %s", Binary, Dir, p.Path, state, strings.Join(why, "; "))
}

// Merge3Way merges two versions of one file of the memory by section, with `agent-memory
// merge-driver`: a section either side added is kept, and a section one side changed is taken from
// that side. clean is false when both sides changed the same section differently: what the program
// wrote then holds both versions between markers, which is of no use to a caller that cannot ask
// anybody, so merged is empty and the caller merges another way.
func Merge3Way(ctx context.Context, rel, base, ours, theirs string) (merged string, clean bool, err error) {
	if err := Require(); err != nil {
		return "", false, err
	}
	dir, err := os.MkdirTemp("", "openspec-memory-merge-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(dir)
	for name, text := range map[string]string{"base.md": base, "ours.md": ours, "theirs.md": theirs} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			return "", false, err
		}
	}
	_, err = run(ctx, dir, "", "merge-driver", "base.md", "ours.md", "theirs.md", filepath.Base(rel))
	var failed *commandError
	var exit *exec.ExitError
	if errors.As(err, &failed) && errors.As(failed.cause, &exit) && exit.ExitCode() > 0 {
		// The program ran to its end and reported sections it could not merge. One that was
		// stopped has no exit code of its own and is an error like any other.
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "ours.md"))
	if err != nil {
		return "", false, err
	}
	return string(raw), true, nil
}

// agent-memory derives two things from the Markdown files: index.md, which git tracks, and a search
// index, which each checkout keeps for itself. Both follow a write that goes through the program in
// that checkout, and neither follows one that does not: after a merge index.md describes the files
// of one side, and a checkout that receives the files through git goes on searching what it held
// before. Normalize and Refresh are the two answers, one for a checkout that is about to commit the
// memory and one for a checkout that is about to be read.

// Normalize has agent-memory bring the memory at root to the form it would have written itself:
// every section with the "@id" line it is found by, index.md describing the files that are there,
// and the search index of this checkout over them. It may change files git tracks, so it belongs
// before a commit of the memory and nowhere else. Every such commit follows it: a memory committed
// in that form is left untouched by any later Normalize or search, in any checkout.
func Normalize(ctx context.Context, root string) error {
	if !Present(root) {
		return nil
	}
	_, err := run(ctx, root, "", "rebuild-index", "--root", root)
	return err
}

// Refresh has agent-memory rebuild the search index of the checkout at root from the files it holds
// now, so that an agent started there finds what arrived through git. It leaves every file git
// tracks as it was: no "@id" line is added, and index.md, which the program writes again when the
// files it lists changed, is put back.
func Refresh(ctx context.Context, root string) error {
	if !Present(root) {
		return nil
	}
	index := filepath.Join(root, filepath.FromSlash(IndexFile))
	before, readErr := os.ReadFile(index)
	_, err := run(ctx, root, "", "rebuild-index", "--assign-ids=false", "--root", root)
	if readErr == nil {
		if after, afterErr := os.ReadFile(index); afterErr != nil || !bytes.Equal(after, before) {
			if writeErr := os.WriteFile(index, before, 0o644); writeErr != nil && err == nil {
				err = writeErr
			}
		}
	}
	return err
}
