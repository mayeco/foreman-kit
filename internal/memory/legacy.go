package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// An application installed before its memory was kept with agent-memory holds it in two places of
// its own: MEMORY.md, read by every agent, and memory/, one file for each repository or capability.
// `foreman-kit update` takes both into .agent-memory/ through agent-memory itself and removes them.

const (
	// LegacyFile is the file an application kept its memory in before agent-memory.
	LegacyFile = "MEMORY.md"
	// LegacyDir is the directory an application kept the notes of one repository or capability in
	// before agent-memory.
	LegacyDir = "memory"
)

var (
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->\n?`)
	heading     = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)
	numbered    = regexp.MustCompile(`^\d+\.\s*`)
	notSlug     = regexp.MustCompile(`[^a-z0-9]+`)
)

// LegacyPresent reports whether the application at root still holds memory from before
// agent-memory.
func LegacyPresent(root string) bool {
	if st, err := os.Stat(filepath.Join(root, LegacyFile)); err == nil && !st.IsDir() {
		return true
	}
	return len(legacyScoped(root)) > 0
}

// legacyScoped returns the Markdown files under memory/, relative to root and sorted.
func legacyScoped(root string) []string {
	var out []string
	_ = filepath.WalkDir(filepath.Join(root, LegacyDir), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		if rel, relErr := filepath.Rel(root, p); relErr == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// slug turns a title or a name into what an "@id" of agent-memory may be.
func slug(text string) string {
	return strings.Trim(notSlug.ReplaceAllString(strings.ToLower(text), "-"), "-")
}

// anchored gives every heading of text the "<!-- @id: ... -->" line agent-memory finds a section
// by, where it has none, each prefixed with prefix and unlike the others.
func anchored(text, prefix string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	used := map[string]bool{}
	for i, line := range lines {
		out = append(out, line)
		m := heading.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if i+1 < len(lines) && strings.Contains(lines[i+1], "@id:") {
			continue
		}
		id := prefix + "-" + slug(m[2])
		for n := 2; used[id]; n++ {
			id = fmt.Sprintf("%s-%s-%d", prefix, slug(m[2]), n)
		}
		used[id] = true
		out = append(out, "<!-- @id: "+id+" -->")
	}
	return strings.Join(out, "\n")
}

// body returns text as it is taken into the memory: without the comments of the template and the
// rules between its sections, and with the names of the files that held it replaced.
func body(text string) string {
	text = htmlComment.ReplaceAllString(text, "")
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "---" {
			continue
		}
		kept = append(kept, line)
	}
	text = strings.TrimSpace(strings.Join(kept, "\n"))
	return strings.ReplaceAll(text, "`"+LegacyFile+"`, `"+LegacyDir+"/`", "`"+Dir+"/`")
}

// legacySection is one "## " section of MEMORY.md.
type legacySection struct {
	title string
	text  string
}

// legacySections splits MEMORY.md at its second-level headings. What precedes the first one says
// how the file was kept, which no longer holds, and is not taken.
func legacySections(text string) []legacySection {
	var out []legacySection
	for _, line := range strings.Split(htmlComment.ReplaceAllString(text, ""), "\n") {
		if strings.HasPrefix(line, "## ") {
			out = append(out, legacySection{title: numbered.ReplaceAllString(strings.TrimSpace(strings.TrimPrefix(line, "## ")), "")})
			continue
		}
		if len(out) > 0 {
			out[len(out)-1].text += line + "\n"
		}
	}
	return out
}

// ImportLegacy takes MEMORY.md and memory/ of the application at root into its .agent-memory/, each
// part through agent-memory: the conventions into conventions.md, the decisions into decisions.md,
// every other section into pitfalls.md, and each file of memory/ into the notes of its repository or
// capability. It returns one line for each part it took. It removes nothing: the caller does, once
// every part is in. today dates the decisions, which had no date.
//
// fresh says the memory was created for this import and holds nothing but the lines agent-memory
// starts a file with, which say what to write there: the first part a file takes stands in their
// place. In a memory that held something already, every part is added to what is there.
func ImportLegacy(ctx context.Context, root string, today time.Time, fresh bool) ([]string, error) {
	var notes []string
	replaced := map[string]bool{}
	// into returns the operation and the text that put text in the section of path it belongs to.
	into := func(path, text string) (op, content string) {
		if fresh && !replaced[path] {
			replaced[path] = true
			return "replace_section_content", "\n" + text + "\n"
		}
		return "append_to_section", text + "\n"
	}
	if raw, err := os.ReadFile(filepath.Join(root, LegacyFile)); err == nil {
		for _, s := range legacySections(string(raw)) {
			text := body(s.text)
			if text == "" {
				continue
			}
			title := strings.ToLower(s.title)
			var p Proposal
			switch {
			case strings.Contains(title, "convention"):
				p = Proposal{Intent: "update_conventions", Path: "conventions.md", SectionID: "conventions"}
				p.Op, p.Content = into(p.Path, anchored(text, "conventions"))
			case strings.Contains(title, "decision"):
				const name = "Decisions recorded before agent-memory"
				p = Proposal{Intent: "record_decision", Op: "append_section", Path: "decisions.md", ParentSectionID: "decisions",
					Heading: name, HeadingLevel: 2,
					Content: fmt.Sprintf("## %s\n<!-- @id: %s -->\n\n**Date:** %s\n**Status:** active\n**Confidence:** user-provided\n\n%s\n",
						name, slug(name), today.UTC().Format("2006-01-02"), anchored(text, slug(name)))}
			default:
				if !strings.Contains(title, "lesson") && !strings.Contains(title, "quirk") && !strings.Contains(title, "pitfall") {
					text = s.title + ":\n\n" + text
				}
				p = Proposal{Intent: "add_pitfall", Path: "pitfalls.md", SectionID: "pitfalls"}
				p.Op, p.Content = into(p.Path, anchored(text, "pitfalls"))
			}
			p.Source, p.Confidence = "file:"+LegacyFile, "user-provided"
			if err := Propose(ctx, root, p); err != nil {
				return notes, fmt.Errorf("%s, section %q: %w", LegacyFile, s.title, err)
			}
			notes = append(notes, fmt.Sprintf("%s, %q -> %s/%s", LegacyFile, s.title, Dir, p.Path))
		}
	}
	for _, rel := range legacyScoped(root) {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return notes, err
		}
		name := strings.TrimSuffix(strings.TrimPrefix(rel, LegacyDir+"/"), ".md")
		var file, title string
		switch {
		case strings.HasPrefix(name, "repos/") && !strings.Contains(strings.TrimPrefix(name, "repos/"), "/"):
			name = strings.TrimPrefix(name, "repos/")
			file, title = RepoModule(name), "Repository "+name
		case strings.HasPrefix(name, "specs/") && !strings.Contains(strings.TrimPrefix(name, "specs/"), "/"):
			name = strings.TrimPrefix(name, "specs/")
			file, title = SpecModule(name), "Capability "+name
		default:
			file, title = ModulesDir+"/"+slug(name)+".md", name
		}
		id := slug(strings.TrimSuffix(filepath.Base(file), ".md"))
		// The file had a title of its own; the notes get the one that names what they are about.
		text := body(string(raw))
		if first, rest, _ := strings.Cut(text, "\n"); strings.HasPrefix(first, "# ") {
			text = strings.TrimSpace(rest)
		}
		p := Proposal{Intent: "refresh_module", Op: "create_file", Path: strings.TrimPrefix(file, Dir+"/"),
			Content: fmt.Sprintf("# %s\n<!-- @id: %s -->\n\n%s\n", title, id, anchored(text, id)),
			Source:  "file:" + rel, Confidence: "user-provided"}
		if err := Propose(ctx, root, p); err != nil {
			return notes, fmt.Errorf("%s: %w", rel, err)
		}
		notes = append(notes, fmt.Sprintf("%s -> %s", rel, file))
	}
	return notes, nil
}
