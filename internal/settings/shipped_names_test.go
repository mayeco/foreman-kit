package settings_test

import (
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/settings"
)

// settingName matches the whole content of an inline code span written as a dotted name.
var settingName = regexp.MustCompile("`([a-z_][a-z0-9_]*(?:\\.[a-z_][a-z0-9_]*)+)`")

// fileExtensions end a dotted token that names a file (`cycle.md`, `repo.config.yaml`).
var fileExtensions = map[string]bool{"md": true, "yaml": true, "yml": true, "json": true, "toml": true, "mod": true, "sh": true, "go": true}

// notSettings are the dotted names of shipped documents that are not keys of the seven settings
// files: each with what it is. Keep it short; a name that is a setting belongs to the catalog.
var notSettings = map[string]string{
	"gates.test":                 "a key of repo.config.yaml",
	"regression.commands":        "a key of repo.config.yaml",
	"regression.commands.test":   "a key of repo.config.yaml",
	"regression.when":            "a key of repo.config.yaml",
	"service.dev_command":        "a key of repo.config.yaml",
	"service.health_endpoint":    "a key of repo.config.yaml",
	"service.port":               "a key of repo.config.yaml",
	"visual_qa.prepare":          "a key of repo.config.yaml",
	"actionable_impact.noul":     "a field of an evaluator result",
	"urgency.choice":             "a field of an evaluator result",
	"exploration_state.history":  "a field of the cycle's state",
	"needs_human.broken_bases":   "a field of the cycle's report",
	"review.apply_error":         "a field of a proposal's review",
	"pending.proposals_approved": "a state of the cycle's report",
	"pending.queue.full":         "a state of the cycle's report",
}

// namesASetting reports whether name is a key of the catalog (bare or with its file), a dotted
// prefix of one, or a key that holds a mapping or a list with a name under it.
func namesASetting(name string) bool {
	parts := strings.Split(name, ".")
	for _, s := range settings.All() {
		for _, full := range []string{string(s.Key), s.Key.Path()} {
			if name == full || strings.HasPrefix(full, name+".") {
				return true
			}
			switch s.Kind {
			case settings.KindCommands, settings.KindArgs, settings.KindList, settings.KindAgents:
				if len(parts) > 1 && strings.HasPrefix(name, full+".") {
					return true
				}
			}
		}
	}
	return false
}

// shippedNameProblem says what is wrong with name as a shipped document writes it, or "".
func shippedNameProblem(name string) string {
	parts := strings.Split(name, ".")
	if fileExtensions[parts[len(parts)-1]] {
		return ""
	}
	if _, ok := notSettings[name]; ok || namesASetting(name) {
		return ""
	}
	return "neither a key of the catalog, a prefix of one nor a listed non-setting token"
}

func TestShippedNameScanJudgesNames(t *testing.T) {
	for _, name := range []string{
		"proposals.approved_by", "cycle.proposals.approved_by", "pipeline.gates", "limits", "commands.hermes",
		"repo.config.yaml", "service.port", "pipeline.gates.timeout", "agents.roles.plan.agents",
	} {
		if name == "limits" {
			continue // a bare word is not a dotted name
		}
		if p := shippedNameProblem(name); p != "" {
			t.Errorf("%s: %s", name, p)
		}
	}
	for _, name := range []string{"proposals.approval", "cycle.proposals.approval", "pipeline.gates.timeouts", "proposals.approved_by.x"} {
		if shippedNameProblem(name) == "" {
			t.Errorf("%s passes the scan, which it must not", name)
		}
	}
}

// TestShippedDocumentsNameOnlySettingsThatExist scans the skills, seeds and templates every
// application receives for the dotted names they write in inline code: a name that is not a key
// of the catalog (a former key included) sends a person or an agent to write a key nothing reads,
// which makes the settings file unusable.
func TestShippedDocumentsNameOnlySettingsThatExist(t *testing.T) {
	var problems []string
	for _, root := range []string{"seeds", "templates", "skills"} {
		files, err := assets.WalkEmbedded(root)
		if err != nil {
			t.Fatal(err)
		}
		for rel, data := range files {
			for _, m := range settingName.FindAllStringSubmatch(string(data), -1) {
				if p := shippedNameProblem(m[1]); p != "" {
					problems = append(problems, path.Join(root, rel)+": `"+m[1]+"` is "+p)
				}
			}
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

func TestProposalApprovalIsNamedAsTheCatalogNamesIt(t *testing.T) {
	want := "`" + settings.ProposalsApprovedBy.Path() + "`"
	for _, file := range []string{"seeds/ROADMAP.md", "skills/foreman-kit-qa/SKILL.md"} {
		data, err := assets.FS.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if !strings.Contains(text, want) {
			t.Errorf("%s does not name %s", file, want)
		}
		if strings.Contains(text, "proposals.approval") {
			t.Errorf("%s still names the former key proposals.approval", file)
		}
	}
}
