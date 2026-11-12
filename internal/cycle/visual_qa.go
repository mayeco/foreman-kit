package cycle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/proc"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/store"
)

const MaxVisualQAPending = 50

var ValidVisualQAVerdicts = map[string]bool{
	"verified":      true,
	"regression":    true,
	"not-visual":    true,
	"cannot-verify": true,
	"passed":        true,
	"failed":        true,
	"skipped":       true,
}

// VisualQAOptions allows customizing external hooks during VisualQABegin / VisualQAEnd.
type VisualQAOptions struct {
	Stamp              string
	SkipWorktreePrep   bool
	ServiceCheckFunc   func(ctx context.Context, root string) error
	ServicePrepareFunc func(ctx context.Context, wt string, slot int) error
}

// VisualQASettings holds the settings of the visual session, from .foreman-kit/visual_qa.yaml.
type VisualQASettings struct {
	Enabled bool
	// TimeBudget is what one session is told it may spend, and ExpiresAfter how long a session that
	// never ended keeps its lock.
	TimeBudget   time.Duration
	ExpiresAfter time.Duration
	// MaxChecks is how many queued checks one session takes, and MaxFindings how many findings it
	// queues or proposes.
	MaxChecks   int
	MaxFindings int
	// Slot is the FOREMANKIT_SLOT of the session's commands (0: none).
	Slot int
	// Skills lists the application's own QA skills the session uses.
	Skills []string
}

// cycleReader reads the settings of root, with .foreman-kit/cycle.yaml taken from cycleCfg when the caller
// already holds it. The other files are read from root.
func cycleReader(root string, cycleCfg map[string]any) *settings.Reader {
	r := settings.At(root)
	if cycleCfg != nil {
		r.Use(settings.CycleBudget.File(), cycleCfg)
	}
	return r
}

// proposalsApproval returns who approves proposals before they are queued. A value the setting does
// not know is read as a human: never as less review than was asked for.
func proposalsApproval(r *settings.Reader) string {
	s := settings.Of(settings.ProposalsApprovedBy)
	raw, _, set := r.Lookup(s.Key)
	if !set {
		return r.String(s.Key)
	}
	if v, ok := s.Parse(raw); ok {
		return v.(string)
	}
	return settings.ApprovalHuman
}

// GetVisualQASettings returns the settings of the visual QA session.
func GetVisualQASettings(root string) VisualQASettings {
	r := settings.At(root)
	return VisualQASettings{
		Enabled:      r.Bool(settings.VisualQAEnabled),
		TimeBudget:   r.Duration(settings.VisualQATimeBudget),
		ExpiresAfter: r.Duration(settings.VisualQAExpiresAfter),
		MaxChecks:    r.Int(settings.VisualQAMaxChecks),
		MaxFindings:  r.Int(settings.VisualQAMaxFindings),
		Slot:         r.Slot(settings.VisualQASlot),
		Skills:       r.Strings(settings.VisualQASkills),
	}
}

// ActiveVisualSession returns the stamp of the currently active visual QA session lock, or "" if none/expired.
func ActiveVisualSession(ctx context.Context, st *store.Store) string {
	if st == nil {
		return ""
	}
	rec, err := st.GetLock(ctx, "visual_qa")
	if err != nil || rec == nil {
		return ""
	}
	now := time.Now().UTC().Unix()
	age := now - rec.AcquiredAt
	if rec.TTLSeconds > 0 && age >= int64(rec.TTLSeconds) {
		return ""
	}
	if rec.Stamp != "" {
		return rec.Stamp
	}
	return "unknown"
}

func nextVisualQAItemID(ctx context.Context, st *store.Store) string {
	items, err := st.ListVisualQA(ctx, "")
	if err != nil || len(items) == 0 {
		return "1"
	}
	maxID := 0
	for _, it := range items {
		if n, err := strconv.Atoi(it.ItemID); err == nil && n > maxID {
			maxID = n
		}
	}
	return strconv.Itoa(maxID + 1)
}

func pruneOldVisualQAPending(ctx context.Context, st *store.Store) {
	items, err := st.ListVisualQA(ctx, "pending")
	if err != nil || len(items) <= MaxVisualQAPending {
		return
	}
	toDrop := len(items) - MaxVisualQAPending
	for i := 0; i < toDrop; i++ {
		_ = st.ResolveVisualQA(ctx, items[i].ItemID, "dropped", "dropped due to queue limit")
	}
}

func getVisualQALastSHA(ctx context.Context, st *store.Store) string {
	if st == nil {
		return ""
	}
	ws, err := st.GetWorkspaceState(ctx)
	if err != nil || ws.CycleStateJSON == "" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(ws.CycleStateJSON), &m) == nil {
		return asString(m["visual_qa_last_sha"])
	}
	return ""
}

func setVisualQALastSHA(ctx context.Context, st *store.Store, sha string) {
	if st == nil {
		return
	}
	_ = st.SetVisualQALastSHA(ctx, sha)
}

func isCommitTerminalOnly(ctx context.Context, root, sha string) bool {
	// -z: paths unquoted (plain output C-quotes non-ASCII names, which then lack the repos/ prefix).
	out, err := gitx.RunGit(ctx, root, "show", "--format=", "--name-only", "-z", "--no-renames", sha)
	if err != nil {
		return false
	}
	seen := make(map[string]bool)
	var repos []string
	for _, line := range strings.Split(out, "\x00") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "repos/") {
			parts := strings.Split(line, "/")
			if len(parts) >= 2 && parts[1] != "" {
				r := "repos/" + parts[1]
				if !seen[r] {
					seen[r] = true
					repos = append(repos, r)
				}
			}
		}
	}
	if len(repos) == 0 {
		return false
	}
	for _, r := range repos {
		cfg := loadYAMLMap(filepath.Join(root, filepath.FromSlash(r), "repo.config.yaml"))
		if asString(cfg["kind"]) != "cli" && !isChildCommitBackendOnly(ctx, root, sha, r) {
			return false
		}
	}
	return true
}

// isChildCommitBackendOnly returns true when a commit in a non-CLI repo (e.g. Rails kind: web)
// only modifies pure backend/DB/API/test paths and touches no UI/views/assets/locales.
func isChildCommitBackendOnly(ctx context.Context, root, umbrellaSHA, repoRel string) bool {
	childDir := filepath.Join(root, filepath.FromSlash(repoRel))
	childSHAOut, err := gitx.RunGit(ctx, root, "rev-parse", "--verify", "--quiet", umbrellaSHA+":"+repoRel)
	if err != nil {
		return false
	}
	childSHA := strings.TrimSpace(childSHAOut)
	if childSHA == "" {
		return false
	}
	var diffOut string
	if parentSHAOut, err := gitx.RunGit(ctx, root, "rev-parse", "--verify", "--quiet", umbrellaSHA+"~1:"+repoRel); err == nil && strings.TrimSpace(parentSHAOut) != "" {
		diffOut, _ = gitx.RunGit(ctx, childDir, "diff", "--name-only", "-z", "--no-renames", strings.TrimSpace(parentSHAOut), childSHA, "--")
	}
	if strings.Trim(diffOut, "\x00\n ") == "" {
		diffOut, _ = gitx.RunGit(ctx, childDir, "show", "--format=", "--name-only", "-z", "--no-renames", childSHA)
	}
	// -z: paths unquoted (plain output C-quotes non-ASCII names, which then match no backend prefix);
	// --no-renames: both sides of a rename, so a view moved into lib/ still counts as a UI change.
	var files []string
	for _, line := range strings.Split(diffOut, "\x00") {
		if f := strings.TrimSpace(filepath.ToSlash(line)); f != "" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return false
	}
	backendPrefixes := []string{
		"db/", "app/models/", "app/jobs/", "app/services/", "app/queries/",
		"app/serializers/", "app/mailers/", "app/controllers/api/",
		"lib/", "spec/", "test/", "config/initializers/", "config/environments/",
	}
	uiSubstrings := []string{
		"app/views/", "app/javascript/", "app/assets/", "app/helpers/", "app/components/",
		"app/controllers/portal/", "app/controllers/public/", "app/controllers/admin/",
		"app/controllers/corporate/", "app/controllers/users/", "config/locales/",
		"src/", "pages/", "components/", "messages/", ".erb", ".html", ".tsx", ".jsx", ".css", ".scss",
	}
	for _, f := range files {
		for _, ui := range uiSubstrings {
			if strings.Contains(f, ui) {
				return false
			}
		}
		isBackend := false
		for _, bp := range backendPrefixes {
			if strings.HasPrefix(f, bp) {
				isBackend = true
				break
			}
		}
		if !isBackend {
			return false
		}
	}
	return true
}

// VisualQAEnqueueIntegrated inspects commits integrated since the last call and enqueues non-CLI changes in visual_qa_items.
func VisualQAEnqueueIntegrated(ctx context.Context, root string, st *store.Store, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	vcfg := GetVisualQASettings(root)
	if !vcfg.Enabled {
		fmt.Fprintf(out, "visual-qa: disabled (%s)\n", settings.VisualQAEnabled.Where())
		return nil
	}
	repo, err := gitx.Open(root)
	if err != nil {
		return err
	}
	head, err := repo.RevParse("HEAD")
	if err != nil {
		return err
	}

	lastSHA := getVisualQALastSHA(ctx, st)
	if lastSHA != "" && !isAncestor(ctx, root, lastSHA, head) {
		lastSHA = ""
	}

	var added []string
	var skipped []string

	if lastSHA != "" && lastSHA != head {
		integrated, err := repo.CompletedSince(lastSHA, head)
		if err == nil {
			existingPending, _ := st.ListVisualQA(ctx, "pending")
			pendingByChange := make(map[string]bool)
			for _, p := range existingPending {
				if p.ChangeName != "" {
					pendingByChange[p.ChangeName] = true
				}
			}
			for _, ic := range integrated {
				if pendingByChange[ic.ChangeName] {
					continue
				}
				if isCommitTerminalOnly(ctx, root, ic.SHA) {
					skipped = append(skipped, ic.ChangeName)
					continue
				}
				shortSHA := ic.SHA
				if len(shortSHA) > 12 {
					shortSHA = shortSHA[:12]
				}
				_, err := st.EnqueueVisualQANext(ctx, store.VisualQAItem{
					ChangeName: ic.ChangeName,
					What:       fmt.Sprintf("integrated in %s", shortSHA),
					Status:     "pending",
					CreatedAt:  time.Now().UTC().Format(time.RFC3339),
				})
				if err == nil {
					pendingByChange[ic.ChangeName] = true
					added = append(added, ic.ChangeName)
				}
			}
			pruneOldVisualQAPending(ctx, st)
		}
	}

	setVisualQALastSHA(ctx, st, head)
	pendingItems, _ := st.ListVisualQA(ctx, "pending")
	note := ""
	if len(skipped) > 0 {
		note = fmt.Sprintf("; %d only touched terminal programs (%s): the headless cycle verifies them", len(skipped), strings.Join(skipped, ", "))
	}
	fmt.Fprintf(out, "visual-qa: %d integrated change(s) queued%s; %d pending\n", len(added), note, len(pendingItems))
	return nil
}

// VisualQARequest enqueues a manual or agent request in visual_qa_items.
func VisualQARequest(ctx context.Context, root string, st *store.Store, what, changeName string, out io.Writer) (*store.VisualQAItem, error) {
	if out == nil {
		out = io.Discard
	}
	what = strings.TrimSpace(what)
	changeName = strings.TrimSpace(changeName)
	if changeName != "" && !ChangeNameRe.MatchString(changeName) {
		return nil, fmt.Errorf("%q is not the name of a change", changeName)
	}
	vcfg := GetVisualQASettings(root)
	if !vcfg.Enabled {
		return nil, fmt.Errorf("visual QA is disabled (%s)", settings.VisualQAEnabled.Where())
	}
	if what == "" {
		return nil, fmt.Errorf("say what to check")
	}
	item := store.VisualQAItem{
		ChangeName: changeName,
		What:       what,
		Status:     "pending",
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	id, err := st.EnqueueVisualQANext(ctx, item)
	if err != nil {
		return nil, err
	}
	item.ItemID = id
	pruneOldVisualQAPending(ctx, st)
	fmt.Fprintf(out, "visual-qa: request %s queued\n", id)
	return &item, nil
}

// VisualQATake claims up to max_checks pending items for session stamp and writes the plan file.
func VisualQATake(ctx context.Context, root string, st *store.Store, stamp, planFile string, out io.Writer) ([]store.VisualQAItem, error) {
	if out == nil {
		out = io.Discard
	}
	vcfg := GetVisualQASettings(root)
	pending, err := st.ListVisualQA(ctx, "pending")
	if err != nil {
		return nil, err
	}
	if limit := vcfg.MaxChecks; limit > 0 && len(pending) > limit {
		pending = pending[:limit]
	}

	for i := range pending {
		_ = st.StartVisualQA(ctx, pending[i].ItemID, "session:"+stamp)
		pending[i].Status = "in_progress"
	}

	stateFile := store.RuntimePath(root, "cycle", "visual_strategy_state.json")
	chosen, _ := PickExplorationStrategy(SessionStrategies(LoadExplorationStrategies(root), settings.SessionVisualQA), LoadExplorationSelection(root), "", stateFile)

	planContent := RenderVisualQAPlan(root, stamp, pending, chosen, nil, vcfg)
	if planFile != "" {
		if err := os.MkdirAll(filepath.Dir(planFile), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(planFile, []byte(planContent), 0o644); err != nil {
			return nil, err
		}
	}

	relPlan := planFile
	if rel, err := filepath.Rel(root, planFile); err == nil && !strings.HasPrefix(rel, "..") {
		relPlan = filepath.ToSlash(rel)
	}
	stratSlug := "none"
	if chosen != nil {
		stratSlug = chosen.Slug
	}
	fmt.Fprintf(out, "visual-qa: started %s plan=%s items=%d strategy=%s\n", stamp, relPlan, len(pending), stratSlug)
	return pending, nil
}

// RenderVisualQAPlan renders the markdown plan for a visual QA session.
func RenderVisualQAPlan(root, stamp string, items []store.VisualQAItem, strategy *ExplorationStrategy, cycleCfg map[string]any, vcfg VisualQASettings) string {
	maxNew := vcfg.MaxFindings
	approval := proposalsApproval(cycleReader(root, cycleCfg))
	var skills []string
	for _, skill := range vcfg.Skills {
		skills = append(skills, "`"+skill+"`")
	}
	skillsLine := fmt.Sprintf("none configured (`%s` in `%s`)", settings.VisualQASkills.Path(), settings.VisualQASkills.FilePath())
	if len(skills) > 0 {
		skillsLine = strings.Join(skills, ", ")
	}

	var checks string
	if len(items) == 0 {
		checks = "Nothing is queued: spend the session on the exploration below."
	} else {
		var lines []string
		for _, it := range items {
			var what string
			if strings.HasPrefix(it.What, "integrated in ") && it.ChangeName != "" {
				what = fmt.Sprintf("change `%s` (%s)", it.ChangeName, it.What)
			} else if it.ChangeName != "" {
				what = fmt.Sprintf("request: %s (change `%s`)", it.What, it.ChangeName)
			} else {
				what = fmt.Sprintf("request: %s", it.What)
			}
			lines = append(lines, fmt.Sprintf("%s. %s", it.ItemID, what))
		}
		checks = strings.Join(lines, "\n")
	}

	var explore string
	if strategy != nil {
		title := strategy.Name
		if title == "" {
			title = strategy.Slug
		}
		kind := strategy.Kind
		if kind == "" {
			kind = "defects"
		}
		explore = fmt.Sprintf("**%s** (`%s`, kind `%s`)\n\n%s", title, strategy.Slug, kind, strings.TrimSpace(strategy.Directive))
	} else {
		explore = "No strategy of `.foreman-kit/exploration.yaml` has `session: " + settings.SessionVisualQA + "`: explore the screens the checks above touched."
	}

	return fmt.Sprintf(`# Visual QA session %s

Written by `+"`foreman-kit visual-qa begin`"+`. Follow the `+"`foreman-kit-visual-qa`"+` skill with this plan, within %d minutes, then run `+"`foreman-kit visual-qa end`"+`.

## Check in the browser

%s

Close each one: `+"`foreman-kit visual-qa done <id> verified|regression|not-visual|cannot-verify \"<one line of evidence>\"`"+`. An item you do not close goes back to the queue.

## Explore

%s

## Findings

- Defects and regressions: backlog items (`+"`foreman-kit-qa`"+`, "Record a finding"); improvements and missing capabilities: proposals in `+"`.foreman-kit/agent_proposals.json`"+` (approval `+"`%s`"+`). At most %d this session.
- Evidence (screenshots, console and network logs): `+"`%s`"+`. Report: `+"`%s`"+`.
- The application's QA skills: %s.
- Lessons: %s
`, stamp, int(vcfg.TimeBudget/time.Minute), checks, explore, approval, maxNew, store.RuntimePath(root, "cycle", "evidence", "visual-"+stamp)+"/", store.RuntimePath(root, "cycle", "reports", "visual-"+stamp+".md"), skillsLine, MemoryPlanLine(root))
}

// VisualQADone marks an item in visual_qa_items as completed with a verdict and note.
func VisualQADone(ctx context.Context, root string, st *store.Store, itemID, verdict, note string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	verdict = strings.TrimSpace(verdict)
	if !ValidVisualQAVerdicts[verdict] {
		return fmt.Errorf("verdict must be one of verified, regression, not-visual, cannot-verify")
	}
	all, err := st.ListVisualQA(ctx, "")
	if err != nil {
		return err
	}
	var target *store.VisualQAItem
	for i := range all {
		if all[i].ItemID == itemID {
			target = &all[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("no item %s", itemID)
	}
	if target.Status != "in_progress" && target.Status != "pending" {
		return fmt.Errorf("item %s is %s, not in progress", itemID, target.Status)
	}
	if err := st.ResolveVisualQA(ctx, itemID, verdict, strings.TrimSpace(note)); err != nil {
		return err
	}
	fmt.Fprintf(out, "visual-qa: %s %s\n", itemID, verdict)
	return nil
}

// VisualQARelease returns unfinished in_progress items back to pending.
func VisualQARelease(ctx context.Context, root string, st *store.Store, stamp string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	all, err := st.ListVisualQA(ctx, "")
	if err != nil {
		return err
	}
	backCount := 0
	doneCount := 0
	for _, it := range all {
		if it.Status == "in_progress" {
			if stamp == "" || it.VerdictNote == "" || it.VerdictNote == "session:"+stamp {
				_ = st.RequeueVisualQA(ctx, it.ItemID)
				backCount++
			}
		} else if it.ResolvedAt != "" && it.Status != "dropped" {
			doneCount++
		}
	}
	fmt.Fprintf(out, "visual-qa: %d checked, %d back to the queue\n", doneCount, backCount)
	return nil
}

// VisualQAStatus prints the current status of the visual QA queue.
func VisualQAStatus(ctx context.Context, root string, st *store.Store, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	vcfg := GetVisualQASettings(root)
	if !vcfg.Enabled {
		fmt.Fprintln(out, "visual QA: disabled")
		return nil
	}
	all, err := st.ListVisualQA(ctx, "")
	if err != nil {
		return err
	}
	pending, inProgress, done := 0, 0, 0
	for _, it := range all {
		switch it.Status {
		case "pending":
			pending++
		case "in_progress":
			inProgress++
		case "dropped":
		default:
			done++
		}
	}
	fmt.Fprintf(out, "visual QA queue: %d pending, %d in progress, %d done\n", pending, inProgress, done)
	return nil
}

// loadRepoConfig returns the repo.config.yaml of the child repository checked out at repoDir, as
// config.ReadRepoConfig reads it (empty when it cannot be read).
func loadRepoConfig(mainRoot, repoDir string) map[string]any {
	out := map[string]any{}
	raw, _, err := config.ReadRepoConfig(mainRoot, repoDir)
	if err != nil {
		return out
	}
	_ = yaml.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func defaultVisualQAServiceCheck(ctx context.Context, root string, st *store.Store) error {
	matches, _ := filepath.Glob(filepath.Join(root, "repos", "*", "repo.config.yaml"))
	sort.Strings(matches)
	for _, cfgPath := range matches {
		if !fileExists(cfgPath) {
			continue
		}
		repoRel := "repos/" + filepath.Base(filepath.Dir(cfgPath))
		repoDir := filepath.Join(root, filepath.FromSlash(repoRel))
		cfg := loadRepoConfig(root, repoDir)
		rawReq, ok := cfg["required_services"]
		if !ok || rawReq == nil {
			continue
		}
		type reqItem struct {
			name string
			cmd  string
		}
		var checks []reqItem
		switch v := rawReq.(type) {
		case map[string]any:
			var keys []string
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				cmd := strings.TrimSpace(asString(v[k]))
				if cmd != "" && !strings.Contains(cmd, "{{") {
					checks = append(checks, reqItem{name: k, cmd: cmd})
				}
			}
		case []any:
			for idx, item := range v {
				cmd := strings.TrimSpace(asString(item))
				if cmd != "" && !strings.Contains(cmd, "{{") {
					checks = append(checks, reqItem{name: fmt.Sprintf("check-%d", idx+1), cmd: cmd})
				}
			}
		}
		for _, ch := range checks {
			res, err := proc.RepoExec(ctx, proc.RepoExecOptions{
				Root:        root,
				Timeout:     30 * time.Second,
				GracePeriod: 10 * time.Second,
				Store:       st,
				Stdout:      io.Discard,
				Stderr:      io.Discard,
			}, repoDir, []string{"bash", "-c", ch.cmd})
			if err != nil || res == nil || res.ExitCode != 0 {
				return fmt.Errorf("a service a repository requires is down: %s:%s", repoRel, ch.name)
			}
		}
	}
	return nil
}

func defaultVisualQAServicePrepare(ctx context.Context, mainRoot string, st *store.Store, visualWT string, slot int) error {
	matches, _ := filepath.Glob(filepath.Join(visualWT, "repos", "*", "repo.config.yaml"))
	sort.Strings(matches)
	for _, cfgPath := range matches {
		if !fileExists(cfgPath) {
			continue
		}
		repoRel := "repos/" + filepath.Base(filepath.Dir(cfgPath))
		repoDir := filepath.Join(visualWT, filepath.FromSlash(repoRel))
		// The clone may hold the file an older foreman-kit read: the one of the main checkout is
		// read then, where foreman-kit update rewrote it.
		cfg := loadRepoConfig(mainRoot, repoDir)
		session, ok := cfg["visual_qa"].(map[string]any)
		if !ok {
			continue
		}
		cmd := strings.TrimSpace(asString(session["prepare"]))
		if cmd == "" || strings.Contains(cmd, "{{") {
			continue
		}
		// The OPENSPEC_ names: the ones a prepare command written before the kit was called Foreman reads.
		extraEnv := []string{"FOREMANKIT_MAIN_ROOT=" + mainRoot, "OPENSPEC_MAIN_ROOT=" + mainRoot}
		if slot > 0 {
			extraEnv = append(extraEnv, fmt.Sprintf("FOREMANKIT_SLOT=%d", slot), fmt.Sprintf("OPENSPEC_SLOT=%d", slot))
		} else {
			extraEnv = append(extraEnv, "FOREMANKIT_SLOT=", "OPENSPEC_SLOT=")
		}
		res, err := proc.RepoExec(ctx, proc.RepoExecOptions{
			Root:     visualWT,
			Slot:     slot,
			ExtraEnv: extraEnv,
			Store:    st,
			Stdout:   io.Discard,
			Stderr:   io.Discard,
		}, repoDir, []string{"bash", "-c", cmd})
		if err != nil || res == nil || res.ExitCode != 0 {
			return fmt.Errorf("visual_qa.prepare of %s failed", repoRel)
		}
	}
	return nil
}

// VisualQABegin opens a visual QA session, acquiring the visual_qa lock in store, preparing the worktree, and claiming items.
func VisualQABegin(ctx context.Context, root string, st *store.Store, opts VisualQAOptions, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if st != nil {
		if ws, err := st.GetWorkspaceState(ctx); err == nil && ws.Paused {
			fmt.Fprintf(out, "visual-qa: paused (%s)\n", ws.PauseReason)
			return nil
		}
	}
	if fileExists(store.RuntimePath(root, "PAUSE")) {
		b, _ := os.ReadFile(store.RuntimePath(root, "PAUSE"))
		line := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
		fmt.Fprintf(out, "visual-qa: paused (%s)\n", line)
		return nil
	}
	if err := RequireUsable(root, settings.VisualSessionFiles...); err != nil {
		fmt.Fprintf(out, "visual-qa: %v\n", err)
		return err
	}

	vcfg := GetVisualQASettings(root)
	if !vcfg.Enabled {
		fmt.Fprintf(out, "visual-qa: disabled (%s)\n", settings.VisualQAEnabled.Where())
		return nil
	}
	if liveStamp := ActiveVisualSession(ctx, st); liveStamp != "" {
		fmt.Fprintf(out, "visual-qa: session %s is still running\n", liveStamp)
		return nil
	}

	return WithRecordLock(ctx, st, 30*time.Minute, func() error {
		if liveStamp := ActiveVisualSession(ctx, st); liveStamp != "" {
			fmt.Fprintf(out, "visual-qa: session %s is still running\n", liveStamp)
			return nil
		}
		if st != nil {
			if oldLock, _ := st.GetLock(ctx, "visual_qa"); oldLock != nil {
				_ = VisualQARelease(ctx, root, st, oldLock.Stamp, io.Discard)
				_ = st.ReleaseLock(ctx, "visual_qa", 0)
			}
		}

		stamp := opts.Stamp
		if stamp == "" {
			stamp = time.Now().UTC().Format("20060102T150405Z")
		}
		ttl := vcfg.ExpiresAfter
		if st != nil {
			_, err := st.AcquireLock(ctx, "visual_qa", store.LockOptions{
				OwnerType: "visual_qa",
				PID:       os.Getpid(),
				Slot:      vcfg.Slot,
				Stamp:     stamp,
				TTL:       ttl,
			})
			if err != nil {
				fmt.Fprintln(out, "visual-qa: another session is starting")
				return nil
			}
		}

		releaseOnErr := func() {
			if st != nil {
				_ = st.ReleaseLock(ctx, "visual_qa", 0)
			}
		}

		if !opts.SkipWorktreePrep && dirExists(filepath.Join(root, ".git")) {
			baseBranch := ResolveBaseBranch(root)
			_ = SyncRepos(ctx, root, st, false, io.Discard)
			baseRef := "HEAD"
			if _, err := gitx.RunGit(ctx, root, "rev-parse", "--verify", "--quiet", "origin/"+baseBranch); err == nil {
				baseRef = "origin/" + baseBranch
			}
			visualWT := filepath.Join(settings.WorktreesBase(root), "_visual_qa")
			if err := PrepareSessionWorktree(ctx, root, visualWT, baseRef, baseBranch, io.Discard); err != nil {
				releaseOnErr()
				return err
			}
			serviceCheck := opts.ServiceCheckFunc
			if serviceCheck == nil {
				serviceCheck = func(ctx context.Context, r string) error {
					return defaultVisualQAServiceCheck(ctx, r, st)
				}
			}
			if err := serviceCheck(ctx, root); err != nil {
				releaseOnErr()
				return err
			}
			servicePrepare := opts.ServicePrepareFunc
			if servicePrepare == nil {
				servicePrepare = func(ctx context.Context, wt string, slot int) error {
					return defaultVisualQAServicePrepare(ctx, root, st, wt, slot)
				}
			}
			if err := servicePrepare(ctx, visualWT, vcfg.Slot); err != nil {
				releaseOnErr()
				return err
			}
		}

		planFile := store.RuntimePath(root, "cycle", fmt.Sprintf("visual-%s.md", stamp))
		if _, err := VisualQATake(ctx, root, st, stamp, planFile, out); err != nil {
			releaseOnErr()
			return err
		}
		return nil
	})
}

// VisualQAEnd closes an active visual QA session, returning unfinished checks, running ScopeGuard, reviewing proposals, and recording.
func VisualQAEnd(ctx context.Context, root string, st *store.Store, opts VisualQAOptions, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	var stamp string
	if st != nil {
		if rec, err := st.GetLock(ctx, "visual_qa"); err == nil && rec != nil {
			stamp = rec.Stamp
		}
	}
	if stamp == "" {
		stamp = opts.Stamp
	}
	if stamp == "" {
		return fmt.Errorf("no session is open")
	}

	_ = VisualQARelease(ctx, root, st, stamp, out)

	visualWT := filepath.Join(settings.WorktreesBase(root), "_visual_qa")

	if dirExists(filepath.Join(root, ".git")) {
		if err := ScopeGuard(ctx, root, "visual QA "+stamp, st, out); err != nil {
			if st != nil {
				_ = st.ReleaseLock(ctx, "visual_qa", 0)
			}
			return err
		}
		if dirExists(visualWT) {
			if err := ScopeGuard(ctx, visualWT, "visual QA "+stamp, st, out); err != nil {
				if st != nil {
					_ = st.ReleaseLock(ctx, "visual_qa", 0)
				}
				return err
			}
		}
	}

	err := WithRecordLock(ctx, st, 30*time.Minute, func() error {
		if dirExists(filepath.Join(root, ".git")) {
			target := root
			edits, _ := ScopeEdits(ctx, root)
			if dirExists(visualWT) {
				target = visualWT
				if edits != "" {
					if err := CarryProposals(ctx, visualWT, root, out); err != nil {
						fmt.Fprintln(out, "visual-qa: could not carry the findings into .worktrees/_visual_qa; they stay in the main checkout")
						target = ""
					}
				}
			}
			if target != "" {
				if err := ReviewProposals(ctx, target, st, nil, out); err != nil {
					fmt.Fprintln(out, "visual-qa: proposal review failed; proposals left as they were")
				}
				baseBranch := ResolveBaseBranch(root)
				if err := RecordSession(ctx, target, baseBranch, "chore(memory): visual QA "+stamp, st, out); err == nil && target == visualWT {
					_ = ReleaseScopeEdits(ctx, root, edits)
				}
			}
		} else {
			_ = ReviewProposals(ctx, root, st, nil, out)
		}
		if st != nil {
			_ = st.ReleaseLock(ctx, "visual_qa", 0)
		}
		// While the session held the main checkout nothing moved it: what this session recorded, and
		// what a cycle published meanwhile, reach it now, and the queue is read from it again.
		if dirExists(filepath.Join(root, ".git")) {
			_ = SyncRepos(ctx, root, st, false, io.Discard)
			if st != nil {
				_ = st.SyncJSONToSQLite(ctx, root)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "visual-qa: session %s closed\n", stamp)
	return nil
}

// RunVisualQACLI implements the CLI entry point for `foreman-kit visual-qa ...`.
func RunVisualQACLI(ctx context.Context, root string, st *store.Store, args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}

	var changeFlag string
	var positional []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--change" && i+1 < len(args) {
			changeFlag = args[i+1]
			i++
		} else if strings.HasPrefix(args[i], "--change=") {
			changeFlag = strings.TrimPrefix(args[i], "--change=")
		} else {
			positional = append(positional, args[i])
		}
	}

	if len(positional) == 0 {
		fmt.Fprintln(stderr, `Usage: foreman-kit visual-qa begin | end | status | enqueue-integrated | request "<what>" [--change NAME] | take <stamp> <plan-file> | done <id> <verdict> ["<note>"] | release <stamp>`)
		return 2
	}

	var err error
	switch positional[0] {
	case "begin":
		err = VisualQABegin(ctx, root, st, VisualQAOptions{}, stdout)
	case "end":
		err = VisualQAEnd(ctx, root, st, VisualQAOptions{}, stdout)
	case "status":
		err = VisualQAStatus(ctx, root, st, stdout)
	case "enqueue-integrated":
		err = VisualQAEnqueueIntegrated(ctx, root, st, stdout)
	case "request":
		if len(positional) < 2 {
			err = fmt.Errorf("say what to check")
		} else {
			_, err = VisualQARequest(ctx, root, st, strings.Join(positional[1:], " "), changeFlag, stdout)
		}
	case "take":
		if len(positional) != 3 {
			fmt.Fprintln(stderr, "Usage: foreman-kit visual-qa take <stamp> <plan-file>")
			return 2
		}
		_, err = VisualQATake(ctx, root, st, positional[1], positional[2], stdout)
	case "done":
		if len(positional) < 3 {
			fmt.Fprintln(stderr, `Usage: foreman-kit visual-qa done <id> <verdict> ["<note>"]`)
			return 2
		}
		note := ""
		if len(positional) > 3 {
			note = strings.Join(positional[3:], " ")
		}
		err = VisualQADone(ctx, root, st, positional[1], positional[2], note, stdout)
	case "release":
		if len(positional) != 2 {
			fmt.Fprintln(stderr, "Usage: foreman-kit visual-qa release <stamp>")
			return 2
		}
		err = VisualQARelease(ctx, root, st, positional[1], stdout)
	default:
		fmt.Fprintln(stderr, `Usage: foreman-kit visual-qa begin | end | status | enqueue-integrated | request "<what>" [--change NAME] | take <stamp> <plan-file> | done <id> <verdict> ["<note>"] | release <stamp>`)
		return 2
	}

	if err != nil {
		fmt.Fprintf(stderr, "visual-qa: %v\n", err)
		return 1
	}
	return 0
}
