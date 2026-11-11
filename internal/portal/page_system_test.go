package portal

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/store"
)

// processesSeed is what seedProcesses recorded.
type processesSeed struct {
	// gonePID is the process of the merger that is gone; ended and running, the two passes of the
	// dispatcher.
	gonePID int
	ended   int64
	running int64
}

// seedProcesses records what the page of processes draws: the lock of a daemon and the lease of a
// change this process holds, the lock a merger that is gone left behind; a pass of the dispatcher
// that ended, whose workers left one change ready and one blocked, and a pass that runs with one
// worker still going; a pause the merger put and a person lifted, and a pause in force.
func seedProcesses(t *testing.T, st *store.Store) processesSeed {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	gone := exec.Command(os.Args[0], "-test.run=^$")
	must(gone.Run())
	seed := processesSeed{gonePID: gone.Process.Pid}

	_, err := st.AcquireLock(ctx, "daemon", store.LockOptions{OwnerType: "daemon", TTL: time.Hour})
	must(err)
	_, err = st.AcquireLock(ctx, store.WorkerLeaseKey("add-ui"), store.LockOptions{OwnerType: "worker", Slot: 2, TTL: 5 * time.Minute})
	must(err)
	_, err = st.AcquireLock(ctx, "merger", store.LockOptions{OwnerType: "merger", PID: seed.gonePID, TTL: time.Hour})
	must(err)

	seed.ended, err = st.BeginDispatcherRun(ctx, "once", 2, 6)
	must(err)
	for change, result := range map[string]string{"add-ui": "ready", "add-api": "blocked"} {
		run, err := st.BeginChangeRun(store.WithDispatcherRun(ctx, seed.ended), change, 1, "/tmp/worker-"+change, "feat/spec-"+change)
		must(err)
		must(st.FinishChangeRun(ctx, run, result))
	}
	must(st.FinishDispatcherRun(ctx, seed.ended, "finished"))
	seed.running, err = st.BeginDispatcherRun(ctx, "continuous", 3, 4)
	must(err)
	_, err = st.BeginChangeRun(store.WithDispatcherRun(ctx, seed.running), "add-ui", 1, "/tmp/worker-add-ui", "feat/spec-add-ui")
	must(err)

	_, err = st.Pause(ctx, "the base branch of backend fails on its own", "merger", "add-api")
	must(err)
	_, err = st.Resume(ctx, "portal")
	must(err)
	_, err = st.Pause(ctx, "maintenance of the build machine", "cli", "")
	must(err)
	return seed
}

// TestProcessesShowsTheLocksThePassesOfTheDispatcherAndThePauses: System → Processes lists the locks
// on record, each with who took it and whether something still holds it, the passes of the
// dispatcher with what their workers did, and the pauses of the workspace, the one in force and the
// ones that were lifted. All of it is rows; the page of before read some of it from the last event
// of the dispatcher and from files.
func TestProcessesShowsTheLocksThePassesOfTheDispatcherAndThePauses(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	seed := seedProcesses(t, st)

	whole, body := overviewPage(t, srv, "/system/processes")
	wantOnPage(t, "the page of processes", body,
		"Processes", "Locks and leases", "Passes of the dispatcher", "Pauses",
		// The tabs of System: the page that held the state of recovery is gone, and this one took its place.
		`href="/system/health"`, `href="/system/settings"`, `href="/system/processes"`,
		// The lock of the daemon and the lease of a change this process holds.
		">daemon<", "the lease of", `href="/changes/add-ui"`, "process "+strconv.Itoa(os.Getpid()), "slot 2",
		// The lock of a merger that is gone: nothing holds it.
		">merger<", "process "+strconv.Itoa(seed.gonePID), "nothing holds it now",
		// The pass that ended: two workers, a backlog of six, and how the changes it took ended.
		"#"+strconv.FormatInt(seed.ended, 10), wordingOf(vocDispatchMode, "once").Text, "finished",
		wordingOf(vocRunResult, "ready").Text, wordingOf(vocRunResult, "blocked").Text,
		// The pass that runs, with a worker still going.
		"#"+strconv.FormatInt(seed.running, 10), wordingOf(vocDispatchMode, "continuous").Text, "running", "still going",
		// The pauses: who put each one, why, and whether it was lifted.
		"maintenance of the build machine", wordingOf(vocPausedBy, "cli").Text, "in force",
		"the base branch of backend fails on its own", wordingOf(vocPausedBy, "merger").Text, `href="/changes/add-api"`, "by "+wordingOf(vocPausedBy, "portal").Text,
	)
	// It keeps itself up to date while a lock is held.
	if !strings.Contains(whole, `data-live-interval="15"`) || !strings.Contains(body, `id="processes-locks" data-live-region`) {
		t.Errorf("the page of processes does not keep itself up to date while a lock is held")
	}
	// The latest pass and the latest pause first.
	if strings.Index(body, "#"+strconv.FormatInt(seed.running, 10)) > strings.Index(body, "#"+strconv.FormatInt(seed.ended, 10)) {
		t.Errorf("the passes of the dispatcher do not come the latest first")
	}
	if strings.Index(body, "maintenance of the build machine") > strings.Index(body, "the base branch of backend fails on its own") {
		t.Errorf("the pauses do not come the latest first")
	}

	// A list at a time: each has its own way to the older ones.
	_, body = overviewPage(t, srv, "/system/processes?limit=1")
	wantOnPage(t, "the page of processes, one of each", body, "#"+strconv.FormatInt(seed.running, 10), "maintenance of the build machine",
		"dispatches_before="+strconv.FormatInt(seed.running, 10), "pauses_before=", "Older")
	wantNotOnPage(t, "the page of processes, one of each", body, "the base branch of backend fails on its own", "finished")
	_, body = overviewPage(t, srv, "/system/processes?limit=1&dispatches_before="+strconv.FormatInt(seed.running, 10))
	wantOnPage(t, "the older passes", body, "#"+strconv.FormatInt(seed.ended, 10), "finished", "maintenance of the build machine", "Latest")

	// The API returns the same read.
	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/system/processes", nil)
	locks, _ := data["locks"].([]any)
	dispatches, _ := data["dispatches"].(map[string]any)
	pauses, _ := data["pauses"].(map[string]any)
	passes, _ := dispatches["items"].([]any)
	stops, _ := pauses["items"].([]any)
	if code != http.StatusOK || len(locks) != 3 || len(passes) != 2 || len(stops) != 2 {
		t.Fatalf("GET /api/system/processes = %d %s", code, raw)
	}
	byKey := map[string]map[string]any{}
	for _, it := range locks {
		l, _ := it.(map[string]any)
		byKey[asStr(l["key"])] = l
	}
	if l := byKey["merger"]; l["stale"] != true || l["owner_type"] != "merger" {
		t.Errorf("the lock of a merger that is gone = %v", l)
	}
	if l := byKey[store.WorkerLeaseKey("add-ui")]; l["stale"] != false || l["change_name"] != "add-ui" || l["slot"] != float64(2) {
		t.Errorf("the lease of a change = %v", l)
	}
	ended, _ := passes[1].(map[string]any)
	results, _ := ended["results"].(map[string]any)
	if ended["id"] != float64(seed.ended) || ended["end_reason"] != "finished" || ended["change_runs"] != float64(2) || results["ready"] != float64(1) || results["blocked"] != float64(1) {
		t.Errorf("the pass that ended = %v", ended)
	}
	if first, _ := stops[0].(map[string]any); first["reason"] != "maintenance of the build machine" || first["paused_by"] != "cli" || first["resumed_at"] != nil {
		t.Errorf("the pause in force = %v", stops[0])
	}
	if second, _ := stops[1].(map[string]any); second["change_name"] != "add-api" || second["resumed_by"] != "portal" || second["resumed_at"] == nil {
		t.Errorf("the pause that was lifted = %v", stops[1])
	}
	for _, path := range []string{"/api/system/processes?limit=0", "/api/system/processes?pauses_before=x", "/api/system/processes?dispatches_before=x"} {
		code, data, raw, _ := doReq(t, srv, http.MethodGet, path, nil)
		if c, _ := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" {
			t.Errorf("GET %s = %d %s", path, code, raw)
		}
	}
}

// seedDaemonGiven makes the daemon of a seed one that was started with `foreman-kit --set`: its lock
// lists what it was given, the way a daemon writes it, and it says again what it had said of its
// next cycle.
func seedDaemonGiven(t *testing.T, st *store.Store, given ...string) {
	t.Helper()
	ctx := context.Background()
	sched, err := st.CycleSchedule(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseLock(ctx, "daemon", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcquireLock(ctx, "daemon", store.LockOptions{OwnerType: "daemon", PID: os.Getpid(), TTL: time.Hour, Metadata: map[string]any{"set": given}}); err != nil {
		t.Fatal(err)
	}
	if sched != nil {
		if err := st.SetCycleSchedule(ctx, os.Getpid(), *sched); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := st.LiveLock(ctx, "daemon")
	if err != nil || lock == nil {
		t.Fatalf("the lock of the daemon: %v, %v", lock, err)
	}
	if read := cycle.GivenToDaemon(lock.MetadataJSON); strings.Join(read, " ") != strings.Join(given, " ") {
		t.Fatalf("the seed no longer writes the lock the way a daemon does: the kit reads %q from it", read)
	}
}

// TestWhatADaemonWasStartedWithIsSaidWhereItRuns: a daemon started with `foreman-kit --set` reads
// values that are in no file. Settings shows what the files say, so Dashboard and Processes say
// what the daemon was given, for as long as it runs. With no daemon, and with one that was given
// nothing, they say nothing of it.
func TestWhatADaemonWasStartedWithIsSaidWhereItRuns(t *testing.T) {
	ctx := context.Background()
	silent := func(t *testing.T, srv *Server, when string) {
		t.Helper()
		for _, path := range []string{"/", "/system/processes"} {
			_, body := overviewPage(t, srv, path)
			wantNotOnPage(t, path+" "+when, body, "--set", "it was given when it started", "Started with")
		}
		for _, path := range []string{"/api/dashboard", "/api/system/processes"} {
			_, data, raw, _ := doReq(t, srv, http.MethodGet, path, nil)
			if list, listed := data["daemon_given"].([]any); !listed || len(list) != 0 {
				t.Errorf("GET %s %s: daemon_given = %v in %s", path, when, data["daemon_given"], raw)
			}
		}
	}

	srv, st, _ := setupTestPortal(t)
	silent(t, srv, "with no daemon")
	if _, err := st.AcquireLock(ctx, "daemon", store.LockOptions{OwnerType: "daemon", PID: os.Getpid(), TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	silent(t, srv, "with a daemon that was given nothing")

	// The same daemon, started with two values.
	given := []string{"evaluator.enabled=false", "pipeline.dispatch.workers=4"}
	seedDaemonGiven(t, st, given...)

	_, body := overviewPage(t, srv, "/")
	wantOnPage(t, "Dashboard with a daemon that was given two values", body,
		"The daemon runs with 2 values it was given when it started", "--set evaluator.enabled=false", "--set pipeline.dispatch.workers=4",
		"No file holds them", `href="/system/processes"`,
	)
	_, body = overviewPage(t, srv, "/system/processes")
	wantOnPage(t, "Processes with a daemon that was given two values", body,
		">daemon<", "Started with", "--set evaluator.enabled=false", "--set pipeline.dispatch.workers=4", "in force until this daemon stops",
	)
	for _, path := range []string{"/api/dashboard", "/api/system/processes"} {
		code, data, raw, _ := doReq(t, srv, http.MethodGet, path, nil)
		list, _ := data["daemon_given"].([]any)
		if code != http.StatusOK || len(list) != 2 || list[0] != given[0] || list[1] != given[1] {
			t.Errorf("GET %s = %d, daemon_given = %v in %s", path, code, data["daemon_given"], raw)
		}
	}
}

// TestProcessesOfAKitThatNeverRan: with nothing on record, each part of the page says what would be
// there, and the page does not refresh itself.
func TestProcessesOfAKitThatNeverRan(t *testing.T) {
	srv, _, _ := setupTestPortal(t)
	whole, body := overviewPage(t, srv, "/system/processes")
	wantOnPage(t, "the page of processes of a kit that never ran", body, "No lock is on record.", "The dispatcher has not run.", "The kit was never paused.")
	if strings.Contains(whole, "data-live-interval") {
		t.Errorf("the page of processes keeps itself up to date with no lock held")
	}
}

// TestHealthShowsTheLastPreflightAndAPersonStartsOne: the preflight is the kit's own, started as an
// operation: the action answers 202 with the execution that runs it, and the page shows the latest
// one a person started, with how it ended and what it printed. One runs at a time.
func TestHealthShowsTheLastPreflightAndAPersonStartsOne(t *testing.T) {
	srv, st, _ := setupTestPortal(t)
	kit := kitOf(srv)

	_, body := overviewPage(t, srv, "/system/health")
	wantOnPage(t, "the page of health with no preflight", body,
		"Health", "No preflight was started from the portal.", `data-post="/api/system/preflight"`, "Run the preflight", "Only the services",
		`href="/agents"`, `href="/system/setup"`)
	// The read says there is none, instead of answering with nothing.
	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/system/health", nil)
	if last, said := data["last"]; code != http.StatusOK || !said || last != nil {
		t.Errorf("GET /api/system/health with no preflight = %d %s", code, raw)
	}

	// The whole preflight: the kit runs it, and the execution keeps what it printed.
	asked := make(chan string, 4)
	kit.operation = func(_ context.Context, _ string, operation string) (string, int, error) {
		asked <- operation
		return "toolchain: OK\nagents: claude OK\npreflight: FAIL (the canary of the evaluator did not answer)\n", 1, nil
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/system/preflight", nil)
	id := asStr(data["exec_id"])
	if code != http.StatusAccepted || id == "" || data["href"] != "/executions/"+id {
		t.Fatalf("POST /api/system/preflight = %d %s", code, raw)
	}
	if op := <-asked; op != "preflight" {
		t.Errorf("the portal asked the kit for %q, want the preflight", op)
	}
	if ex := waitExecution(t, st, id); ex.Status != "failed" || ex.JobKind != "preflight" || ex.CommandText != "foreman-kit preflight" {
		t.Errorf("the execution of the preflight = %+v", ex)
	}
	whole, body := overviewPage(t, srv, "/system/health")
	wantOnPage(t, "the page of health after a preflight that failed", body,
		wordingOf(vocExecutionStatus, "failed").Text, "the whole preflight", `href="/executions/`+id+`"`,
		"preflight: FAIL (the canary of the evaluator did not answer)", "Run the preflight")
	wantNotOnPage(t, "the page of health after a preflight that ended", body, "No preflight was started", "Follow the preflight that runs")
	if strings.Contains(whole, "data-live-interval") {
		t.Errorf("the page of health keeps itself up to date with no preflight running")
	}

	// The services alone, and the page while it runs: it offers to follow it, not to start another.
	release := make(chan struct{})
	kit.operation = func(_ context.Context, _ string, operation string) (string, int, error) {
		asked <- operation
		<-release
		return "services: OK\n", 0, nil
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/system/preflight", map[string]any{"services_only": true})
	services := asStr(data["exec_id"])
	if code != http.StatusAccepted || services == "" || services == id {
		t.Fatalf("POST /api/system/preflight for the services = %d %s", code, raw)
	}
	if op := <-asked; op != "preflight-services" {
		t.Errorf("the portal asked the kit for %q, want the services alone", op)
	}
	whole, body = overviewPage(t, srv, "/system/health")
	wantOnPage(t, "the page of health while a preflight runs", body, wordingOf(vocExecutionStatus, "running").Text, "the services alone", "running for",
		"Follow the preflight that runs", `href="/executions/`+services+`"`)
	wantNotOnPage(t, "the page of health while a preflight runs", body, `data-post="/api/system/preflight"`)
	if !strings.Contains(whole, `data-live-interval="5"`) {
		t.Errorf("the page of health does not keep itself up to date while a preflight runs")
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/system/preflight", nil)
	if c, msg := errorOf(data); code != http.StatusConflict || c != "conflict" || !strings.Contains(msg, "/executions/"+services) {
		t.Errorf("a second preflight while one runs = %d %s", code, raw)
	}
	close(release)
	if ex := waitExecution(t, st, services); ex.Status != "succeeded" || ex.CommandText != "foreman-kit preflight --services" {
		t.Errorf("the execution of the check of the services = %+v", ex)
	}

	// The API returns the same read, and the action refuses what it does not take.
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/system/health", nil)
	last, _ := data["last"].(map[string]any)
	output, _ := data["output"].(map[string]any)
	if code != http.StatusOK || last["exec_id"] != services || last["status"] != "succeeded" || !strings.Contains(asStr(output["content"]), "services: OK") {
		t.Errorf("GET /api/system/health = %d %s", code, raw)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/system/preflight", map[string]any{"services": true})
	if c, _ := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" {
		t.Errorf("a preflight asked with a field the action has not = %d %s", code, raw)
	}
	select {
	case op := <-asked:
		t.Errorf("the kit was asked for %q once more than a person asked", op)
	default:
	}
}

// TestSetupShowsWhatIsLeftToDoAndAPersonPreparesARepository: System → Setup is the checklist of the
// kit, each thing done or to do with what does it; POST /api/system/repos prepares a repository
// that is under repos/, and refuses what it cannot use.
func TestSetupShowsWhatIsLeftToDoAndAPersonPreparesARepository(t *testing.T) {
	srv, st, root := setupTestPortal(t)

	_, body := overviewPage(t, srv, "/system/setup")
	wantOnPage(t, "the page of setup", body,
		"Setup", "left to do", "Next:", "The kit", "The child repositories", "Add or prepare a child repository",
		"What makes its cycles run",
		`data-api="/api/system/repos"`, `name="url"`, `name="name"`, `name="branch"`, `name="force"`,
		// The sync of the repositories, alone and with what it publishes, which asks first.
		"Bring the repositories up to date", `data-post="/api/repos/sync"`, `{"publish":false}`, `{"publish":true}`, "Sync and publish?",
		"Neither is done while a visual QA session is in progress",
		`href="/system/health"`, `href="/system/processes"`)

	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/system/setup", nil)
	sections, _ := data["sections"].([]any)
	if code != http.StatusOK || len(sections) == 0 || data["base_branch"] == "" {
		t.Fatalf("GET /api/system/setup = %d %s", code, raw)
	}
	// What makes the cycles run is what the kit gives, whichever way it gives: the page shows it.
	kitSays, err := cycle.SetupStatus(context.Background(), root, st)
	if err != nil {
		t.Fatal(err)
	}
	given, _ := kitSays["schedule"].(map[string]any)
	schedule, _ := data["schedule"].(map[string]any)
	shown := 0
	for _, way := range []string{"daemon", "crontab"} {
		line := asStr(given[way])
		if asStr(schedule[way]) != line {
			t.Errorf("the setup gives %q as the %s of the application, and the kit says %q", schedule[way], way, line)
		}
		if line != "" {
			shown++
			if !strings.Contains(line, root) || !strings.Contains(textOf(body), line) {
				t.Errorf("the page of setup does not show the %s the kit gives: %q", way, line)
			}
		}
	}
	if shown == 0 {
		t.Errorf("the kit says nothing of what makes the cycles run: %v", kitSays["schedule"])
	}
	todo, first := 0, ""
	for _, it := range sections {
		section, _ := it.(map[string]any)
		items, _ := section["items"].([]any)
		if asStr(section["name"]) == "" || asStr(section["title"]) == "" || len(items) == 0 {
			t.Errorf("a part of the setup with no name, no title or nothing in it: %v", section)
		}
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			if item["what"] == "" {
				t.Errorf("a thing of the setup that does not say what it is: %v", item)
			}
			// What is done names no command, and what is left names the one that does it.
			if done, _ := item["done"].(bool); !done {
				todo++
				if first == "" {
					first = asStr(item["what"])
				}
				if item["fix"] == nil {
					t.Errorf("a thing left to do does not say what does it: %v", item)
				}
				if !strings.Contains(textOf(body), asStr(item["what"])) {
					t.Errorf("the page of setup does not show %q", item["what"])
				}
			} else if item["fix"] != nil {
				t.Errorf("a thing that is done names a command: %v", item)
			}
		}
	}
	if data["todo"] != float64(todo) || todo == 0 {
		t.Errorf("the setup says %v things are left to do, and its parts hold %d", data["todo"], todo)
	}
	// What is next is the first thing left to do, in the order a person works through the parts.
	if next, _ := data["next"].(map[string]any); asStr(next["what"]) != first || asStr(next["fix"]) == "" {
		t.Errorf("the setup says %v is next, and the first thing left to do is %q", data["next"], first)
	}

	// A repository that is under repos/ is prepared: it gets its repo.config.yaml.
	repo := filepath.Join(root, "repos", "payments")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	code, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/system/repos", map[string]any{"name": "payments"})
	if code != http.StatusOK || !strings.Contains(asStr(data["output"]), "repos/payments/repo.config.yaml") {
		t.Fatalf("POST /api/system/repos for a repository that is there = %d %s", code, raw)
	}
	written, err := os.ReadFile(filepath.Join(repo, "repo.config.yaml"))
	if err != nil || len(written) == 0 {
		t.Fatalf("the repository was not prepared: %v", err)
	}
	// Asked again, it keeps the file a person may have edited; with force it writes it again.
	if err := os.WriteFile(filepath.Join(repo, "repo.config.yaml"), []byte("# edited by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/system/repos", map[string]any{"name": "payments"}); !strings.Contains(asStr(data["output"]), "kept") {
		t.Errorf("preparing a repository again = %s", raw)
	}
	if kept, _ := os.ReadFile(filepath.Join(repo, "repo.config.yaml")); string(kept) != "# edited by hand\n" {
		t.Errorf("preparing a repository again rewrote its file")
	}
	if _, data, raw, _ = doReq(t, srv, http.MethodPost, "/api/system/repos", map[string]any{"name": "payments", "force": true}); !strings.Contains(asStr(data["output"]), "wrote") {
		t.Errorf("preparing a repository with force = %s", raw)
	}
	if again, _ := os.ReadFile(filepath.Join(repo, "repo.config.yaml")); string(again) != string(written) {
		t.Errorf("force did not write the file of the repository again")
	}

	// What the action refuses, each as what was asked for and not as a failure of the portal.
	for name, in := range map[string]map[string]any{
		"nothing":                            {},
		"a repository that is not there":     {"name": "no-such-repo"},
		"a name that is not of a repository": {"name": "../outside"},
		"a branch with no repository to add": {"name": "payments", "branch": "main"},
		"a URL that is an option of git":     {"url": "--upload-pack=touch /tmp/x", "name": "evil"},
		"a field the action does not take":   {"name": "payments", "path": "repos/payments"},
	} {
		code, data, raw, _ := doReq(t, srv, http.MethodPost, "/api/system/repos", in)
		if c, _ := errorOf(data); code != http.StatusBadRequest || c != "invalid_input" {
			t.Errorf("%s: POST /api/system/repos = %d %s", name, code, raw)
		}
	}
}

// TestPromptTemplatesComeFromTheDefinitionsOfTheKit: System → Prompt templates lists the prompt of
// each role of the kit, with the version the kit records with each call and the placeholders the
// kit fills for the role. The page of one shows its file and a sample of what an agent is given,
// and says which placeholder the file lacks or writes without the kit knowing it.
func TestPromptTemplatesComeFromTheDefinitionsOfTheKit(t *testing.T) {
	srv, _, root := setupTestPortal(t)

	code, data, raw, _ := doReq(t, srv, http.MethodGet, "/api/system/prompts", nil)
	templates, _ := data["templates"].([]any)
	if code != http.StatusOK || len(templates) != len(config.RolesOrder) {
		t.Fatalf("GET /api/system/prompts = %d %s", code, raw)
	}
	if errs, _ := data["errors"].([]any); len(errs) != 0 {
		t.Errorf("the check finds errors in the prompts the kit installs: %v", errs)
	}
	_, list := overviewPage(t, srv, "/system/prompts")
	for i, role := range config.RolesOrder {
		got, _ := templates[i].(map[string]any)
		path := config.PromptsDir + "/" + role + ".md"
		if got["role"] != role || got["path"] != path || got["about"] != config.PromptRoleDescriptions[role] || got["missing"] != false || len(asStr(got["version"])) != 12 {
			t.Errorf("the prompt %d reads %v, want the one of %s", i, got, role)
		}
		if got["raw"] != nil || got["sample"] != nil {
			t.Errorf("the list of prompts carries the file of %s", role)
		}
		holders, _ := got["placeholders"].([]any)
		order := config.PlaceholdersOrder[role]
		if len(holders) != len(order) {
			t.Fatalf("the prompt of %s lists %d placeholders, and the kit fills %d", role, len(holders), len(order))
		}
		required := map[string]bool{}
		for _, name := range config.REQUIRED[role] {
			required[name] = true
		}
		for j, name := range order {
			h, _ := holders[j].(map[string]any)
			if h["name"] != name || h["holds"] != config.PLACEHOLDERS[role][name] || h["required"] != required[name] {
				t.Errorf("the placeholder %d of %s reads %v, want %s", j, role, h, name)
			}
			if required[name] && h["used"] != true {
				t.Errorf("the prompt of %s the kit installs does not write {{%s}}, which it must", role, name)
			}
		}
		wantOnPage(t, "the list of prompts", list, strings.ToUpper(role), path, config.PromptRoleDescriptions[role], asStr(got["version"]),
			`href="/system/prompts/`+role+`"`, `href="/system/prompts/`+role+`?view=sample"`)
	}

	// One prompt: its file, and what an agent would be given.
	code, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/system/prompts/build", nil)
	file, err := os.ReadFile(config.PromptPath(root, "build"))
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK || data["raw"] != string(file) {
		t.Fatalf("GET /api/system/prompts/build = %d %s", code, raw)
	}
	if sample := asStr(data["sample"]); !strings.Contains(sample, "example-change") || !strings.Contains(sample, "feat/spec-example-change") || strings.Contains(sample, "{{change}}") {
		t.Errorf("the sample of the prompt of build does not fill its placeholders:\n%s", sample)
	}
	_, body := overviewPage(t, srv, "/system/prompts/build")
	wantOnPage(t, "the prompt of build", body, "BUILD prompt", config.PromptRoleDescriptions["build"], config.PromptsDir+"/build.md",
		"{{branch}}", config.PLACEHOLDERS["build"]["branch"], "written, as it must", "Template", "Sample render", `id="prompt-raw"`)
	_, body = overviewPage(t, srv, "/system/prompts/build?view=sample")
	wantOnPage(t, "the sample of the prompt of build", body, `id="prompt-sample"`, "example-change")

	// A way to show it the page does not have, and a role the kit does not have.
	for address, want := range map[string]int{"/system/prompts/build?view=whole": http.StatusBadRequest, "/system/prompts/review": http.StatusNotFound} {
		if code, _, _ := getPage(t, srv, address); code != want {
			t.Errorf("GET %s = %d, want %d", address, code, want)
		}
		if code, _, raw, _ := doReq(t, srv, http.MethodGet, "/api"+address, nil); code != want {
			t.Errorf("GET /api%s = %d %s, want %d", address, code, raw, want)
		}
	}

	// A prompt that lacks a placeholder it must write, and writes one the kit does not fill.
	planPath := config.PromptPath(root, "plan")
	plan, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.ReplaceAll(string(plan), "{{feedback}}", "") + "\nSee {{ticket}}.\n"
	if err := os.WriteFile(planPath, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	_, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/system/prompts", nil)
	if errs, _ := data["errors"].([]any); len(errs) != 2 {
		t.Errorf("the check of a prompt with two mistakes says %v: %s", data["errors"], raw)
	}
	_, list = overviewPage(t, srv, "/system/prompts")
	wantOnPage(t, "the list with a prompt that is wrong", list, "The check of the prompts found 2 errors", "{{feedback}} is missing", "{{ticket}} is unknown")
	_, body = overviewPage(t, srv, "/system/prompts/plan")
	wantOnPage(t, "a prompt that is wrong", body, "missing: the prompt must write it", "{{ticket}}", "the kit does not know it")

	// A prompt that is not there.
	if err := os.Remove(config.PromptPath(root, "cycle")); err != nil {
		t.Fatal(err)
	}
	_, data, raw, _ = doReq(t, srv, http.MethodGet, "/api/system/prompts/cycle", nil)
	if data["missing"] != true || asStr(data["version"]) != "" || data["raw"] != nil {
		t.Errorf("a prompt that is not there reads %s", raw)
	}
	_, body = overviewPage(t, srv, "/system/prompts/cycle")
	wantOnPage(t, "a prompt that is not there", body, "is missing from this checkout", "There is no file to show.")
	_, list = overviewPage(t, srv, "/system/prompts")
	wantOnPage(t, "the list with a prompt that is not there", list, "missing")
}
