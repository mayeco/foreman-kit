package portal

import (
	"context"
	"os"
	"strings"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/cycle"
)

// What the portal knows of the installation of the kit in this application outside the store: the
// checklist of its setup, as the kit works it out, and the repositories a person adds to it.

// SetupItem is one thing the setup of an application needs: done, or to do with what does it.
type SetupItem struct {
	Done bool   `json:"done"`
	What string `json:"what"`
	// Fix is the command, or the step, that does it; "" for one that is done.
	Fix string `json:"fix,omitempty"`
}

// SetupSection is one part of the setup with what it needs: Name is the name the kit gives it, and
// Title what a page calls it.
type SetupSection struct {
	Name  string      `json:"name"`
	Title string      `json:"title"`
	Items []SetupItem `json:"items"`
}

// SetupReport is the checklist that takes an application from installed to ready to schedule.
type SetupReport struct {
	BaseBranch string         `json:"base_branch"`
	Sections   []SetupSection `json:"sections"`
	// Todo counts what is left to do, and Next is the first of it: nil when nothing is left.
	Todo int        `json:"todo"`
	Next *SetupItem `json:"next,omitempty"`
	// Schedule is how the kit says the cycles of this application are made to run: the command that
	// starts its daemon, or the line of crontab that starts a cycle. Either is "" when the kit does
	// not give it.
	Schedule SetupSchedule `json:"schedule"`
}

// SetupSchedule is what makes the cycles of an application run, as the kit gives it.
type SetupSchedule struct {
	Daemon  string `json:"daemon,omitempty"`
	Crontab string `json:"crontab,omitempty"`
}

// setupSections are the parts of the setup, in the order a person works through them, each with
// what a page calls it.
var setupSections = []struct{ name, title string }{
	{"kit", "The kit"},
	{"project", "The files of the project"},
	{"repositories", "The child repositories"},
	{"settings", "The settings"},
	{"commit", "What is committed"},
	{"run", "The first cycle"},
}

// setupRead is the checklist of the setup as the kit returned it, or why it could not.
type setupRead struct {
	raw map[string]any
	err error
}

// Setup asks the kit for the checklist of the setup of this application, as `foreman-kit setup`
// prints it. The kit asks git about the application to work it out: the pages that ask at the same
// time share one run of it.
func (w *workspace) Setup(ctx context.Context) (SetupReport, error) {
	read := w.setups.get(func() setupRead {
		// The run is shared: it is not cut short when the request that began it goes away.
		raw, err := cycle.SetupStatus(context.WithoutCancel(ctx), w.root, w.store)
		return setupRead{raw: raw, err: err}
	})
	raw, err := read.raw, read.err
	if err != nil {
		return SetupReport{}, err
	}
	rep := SetupReport{BaseBranch: asStr(raw["base_branch"]), Sections: []SetupSection{}}
	if schedule, ok := raw["schedule"].(map[string]any); ok {
		rep.Schedule = SetupSchedule{Daemon: asStr(schedule["daemon"]), Crontab: asStr(schedule["crontab"])}
	}
	sections, _ := raw["sections"].(map[string]any)
	for _, part := range setupSections {
		items, _ := sections[part.name].([]map[string]any)
		if len(items) == 0 {
			continue
		}
		section := SetupSection{Name: part.name, Title: part.title}
		for _, it := range items {
			item := SetupItem{Done: asStr(it["state"]) == "ok", What: asStr(it["what"])}
			if !item.Done {
				item.Fix = asStr(it["fix"])
				rep.Todo++
				if rep.Next == nil {
					next := item
					rep.Next = &next
				}
			}
			section.Items = append(section.Items, item)
		}
		rep.Sections = append(rep.Sections, section)
	}
	return rep, nil
}

// SetupRepository adds a child repository to the application, or prepares one that is there. With a
// URL, the repository is added under repos/ (as name, or as the URL names it) on branch, and then
// prepared; with a name alone, the repository of that name gets its repo.config.yaml, written again
// over the one it has when force says so. It answers with what the kit printed.
func (w *workspace) SetupRepository(ctx context.Context, url, name, branch string, force bool) (string, error) {
	url, name, branch = strings.TrimSpace(url), strings.TrimSpace(name), strings.TrimSpace(branch)
	if url == "" && name == "" {
		return "", errInvalid("url names the repository to add, or name the one under repos/ to prepare: both are empty")
	}
	if url == "" && branch != "" {
		return "", errInvalid("branch is the branch of a repository that is added: it goes with url")
	}
	w.files.Lock()
	defer w.files.Unlock()
	var out strings.Builder
	var err error
	if url != "" {
		err = cycle.SetupAddRepo(ctx, w.root, url, name, branch, &out)
	} else {
		err = cycle.SetupRepo(w.root, name, force, &out)
	}
	if err != nil {
		// What the kit refuses here is what was asked for: a name or a URL it cannot use, a
		// repository that is not there, or one git could not fetch.
		return out.String(), errInvalid("the kit did not do it: %v", err)
	}
	return strings.TrimSpace(out.String()), nil
}

// ---- the prompts ---------------------------------------------------------------------------------------

// PromptPlaceholder is one {{placeholder}} the prompt of a role may write, as the kit defines it.
type PromptPlaceholder struct {
	Name string `json:"name"`
	// Holds says what the kit writes in its place on each call.
	Holds string `json:"holds"`
	// Required says the kit refuses a prompt of the role that does not write it; Used, that the file
	// of the application writes it.
	Required bool `json:"required"`
	Used     bool `json:"used"`
}

// PromptTemplate is the prompt of one role: the file of the application an agent is given on every
// call of that role, with its placeholders filled in.
type PromptTemplate struct {
	Role string `json:"role"`
	// About says what the agent of the role does, and Path where its prompt is.
	About string `json:"about"`
	Path  string `json:"path"`
	// Missing says the application does not have the file.
	Missing bool `json:"missing"`
	// Version is what the kit records with each call made with this prompt: it changes when the
	// text an agent is given changes. It is "" for a file that is missing.
	Version string `json:"version"`
	// Placeholders are the ones the kit fills for the role, in its order; Unknown, the ones the file
	// writes that the kit does not fill, which reach the agent as they are written.
	Placeholders []PromptPlaceholder `json:"placeholders"`
	Unknown      []string            `json:"unknown"`
	// Raw is the file as it is, and Sample what an agent would be given, with example values in the
	// place of what the kit writes. Both are read for one prompt, not for the list of them.
	Raw    string `json:"raw,omitempty"`
	Sample string `json:"sample,omitempty"`
}

// PromptsReport is the prompts of the application with what the check of the kit says of them.
type PromptsReport struct {
	Templates []PromptTemplate `json:"templates"`
	Errors    []string         `json:"errors"`
	Warnings  []string         `json:"warnings"`
}

// promptOf describes the prompt of role as the application holds it. With whole it also reads the
// file and fills a sample of it.
func (w *workspace) promptOf(role string, whole bool) PromptTemplate {
	t := PromptTemplate{
		Role: role, About: config.PromptRoleDescriptions[role], Path: config.PromptsDir + "/" + role + ".md",
		Placeholders: []PromptPlaceholder{}, Unknown: []string{},
	}
	used := map[string]bool{}
	names, err := config.Placeholders(w.root, role)
	if err != nil {
		t.Missing = true
	}
	for _, name := range names {
		used[name] = true
	}
	required := map[string]bool{}
	for _, name := range config.REQUIRED[role] {
		required[name] = true
	}
	known := config.PLACEHOLDERS[role]
	for _, name := range config.PlaceholdersOrder[role] {
		t.Placeholders = append(t.Placeholders, PromptPlaceholder{Name: name, Holds: known[name], Required: required[name], Used: used[name]})
	}
	for _, name := range names {
		if _, ok := known[name]; !ok {
			t.Unknown = append(t.Unknown, name)
		}
	}
	if t.Missing {
		return t
	}
	if v, err := config.Version(w.root, role); err == nil {
		t.Version = v
	}
	if !whole {
		return t
	}
	if raw, err := os.ReadFile(config.PromptPath(w.root, role)); err == nil {
		t.Raw = string(raw)
	}
	// What an agent would be given: example values in the place of what a change brings, and the
	// memory and the rules of this application as they are.
	rules, _ := assets.ReadTemplate(w.root, config.RulesTemplate)
	sample := map[string]string{
		"change":          "example-change",
		"goal":            "Example backlog item description",
		"branch":          "feat/spec-example-change",
		"memory":          cycle.MemoryPromptBlock(w.root, nil),
		"evaluator_hint":  "",
		"authoring_rules": string(rules),
		"feedback":        "",
		"rebuild":         "",
	}
	t.Sample, _ = config.Render(w.root, role, sample)
	return t
}

// Prompts lists the prompts of the application, one for each role of the kit, with what the check
// of the kit says of them.
func (w *workspace) Prompts() PromptsReport {
	rep := PromptsReport{Templates: []PromptTemplate{}}
	for _, role := range config.RolesOrder {
		rep.Templates = append(rep.Templates, w.promptOf(role, false))
	}
	problems := config.Problems(w.root)
	rep.Errors, rep.Warnings = orEmpty(problems.Errors), orEmpty(problems.Warnings)
	return rep
}

// Prompt returns the prompt of one role with its file and a sample of what an agent is given. It
// is 404 for a role the kit does not have.
func (w *workspace) Prompt(role string) (PromptTemplate, error) {
	for _, known := range config.RolesOrder {
		if known == role {
			return w.promptOf(role, true), nil
		}
	}
	return PromptTemplate{}, errNotFound("there is no prompt of a role %q: the roles are %s", role, strings.Join(config.RolesOrder, ", "))
}
