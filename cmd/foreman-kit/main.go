package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mayeco/foreman-kit/internal/assets"
	"github.com/mayeco/foreman-kit/internal/config"
	"github.com/mayeco/foreman-kit/internal/cycle"
	"github.com/mayeco/foreman-kit/internal/evaluator"
	"github.com/mayeco/foreman-kit/internal/gitx"
	"github.com/mayeco/foreman-kit/internal/memory"
	"github.com/mayeco/foreman-kit/internal/orchestrator"
	"github.com/mayeco/foreman-kit/internal/portal"
	"github.com/mayeco/foreman-kit/internal/proc"
	"github.com/mayeco/foreman-kit/internal/settings"
	"github.com/mayeco/foreman-kit/internal/sim"
	"github.com/mayeco/foreman-kit/internal/store"
)

type exitCodeError struct {
	code int
}

func (e exitCodeError) Error() string {
	return fmt.Sprintf("exit status %d", e.code)
}

func main() {
	// Every repository this process works on is named by a directory, never by the environment: started
	// from a git hook it inherits GIT_DIR and GIT_INDEX_FILE, which must not reach its agents and gates.
	gitx.DropRepoLocalEnv()
	adoptFormerNames()
	surviveBrokenPipe()
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()
	if cwd, err := os.Getwd(); err == nil {
		loadDotenv(cwd, os.Stderr)
	}
	os.Exit(Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// NewRootCmd constructs the Cobra root command and all foreman-kit subcommands.
func NewRootCmd(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}

	var (
		rootDaemon         bool
		rootWeb            bool
		rootPort           int
		rootBind           string
		rootInterval       time.Duration
		rootNoInitialCycle bool
	)

	rootCmd := &cobra.Command{
		Use:           "foreman-kit",
		Short:         "OpenSpec autonomous agent orchestration kit",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       fmt.Sprintf("%s (%s)", assets.Version, assets.SourceCommit),
	}
	rootCmd.SetOut(stdout)
	rootCmd.SetErr(stderr)
	rootCmd.Flags().BoolVarP(&rootDaemon, "daemon", "d", false, "Start the per-project background daemon in the current directory")
	rootCmd.Flags().BoolVarP(&rootWeb, "web", "w", false, "Start the embedded web portal (with -d for background daemon, or standalone)")
	rootCmd.Flags().IntVar(&rootPort, "port", 0, "TCP port for the embedded web portal (default 4567)")
	rootCmd.Flags().StringVar(&rootBind, "bind", "", "Bind address for the embedded web portal (default 127.0.0.1)")
	rootCmd.Flags().DurationVar(&rootInterval, "interval", 0, "Interval between scheduled cycles inside the daemon (default 60m)")
	rootCmd.Flags().BoolVar(&rootNoInitialCycle, "no-initial-cycle", false, "Do not run an immediate cycle upon daemon startup")

	openWorkspaceStore := func() (string, *store.Store, error) {
		root := config.AppRoot("")
		st, err := store.Open(root)
		return root, st, err
	}

	openTargetStore := func(dirArg string) (string, *store.Store, error) {
		dirArg = strings.TrimSpace(dirArg)
		if dirArg != "" {
			if abs, err := filepath.Abs(dirArg); err == nil {
				dirArg = abs
			}
			prev, hadPrev := os.LookupEnv("FOREMANKIT_APP_ROOT")
			_ = os.Setenv("FOREMANKIT_APP_ROOT", dirArg)
			defer func() {
				if hadPrev {
					_ = os.Setenv("FOREMANKIT_APP_ROOT", prev)
				} else {
					_ = os.Unsetenv("FOREMANKIT_APP_ROOT")
				}
			}()
			root := config.AppRoot(dirArg)
			st, err := store.Open(root)
			return root, st, err
		}
		return openWorkspaceStore()
	}

	wrapCode := func(code int) error {
		if code == 0 {
			return nil
		}
		return exitCodeError{code: code}
	}

	buildCycleOptions := func(st *store.Store) cycle.CycleOptions {
		return cycle.CycleOptions{
			PreflightFunc: func(ctx context.Context, r string, out io.Writer) error {
				code := proc.RunPreflightCLI(ctx, r, st, nil, out, out)
				if code != 0 {
					return fmt.Errorf("preflight exited %d", code)
				}
				return nil
			},
			MergeFunc: func(ctx context.Context, r string, out io.Writer) error {
				m, err := orchestrator.NewMerger(orchestrator.MergerOptions{
					Root:  r,
					Store: st,
				})
				if err != nil {
					return err
				}
				defer m.Close()
				return m.Run(ctx)
			},
			AgentExecFunc: func(ctx context.Context, workdir, promptFile, engine string, timeout time.Duration, out io.Writer) error {
				res, err := proc.AgentExec(ctx, proc.AgentExecOptions{
					Workdir:        workdir,
					PromptFile:     promptFile,
					Engines:        engine,
					Slot:           98,
					TimeoutSeconds: int(timeout.Seconds()),
					Store:          st,
					Stdout:         out,
					Stderr:         out,
				})
				if res != nil && res.ExitCode != 0 {
					return fmt.Errorf("agent_exec exited %d", res.ExitCode)
				}
				return err
			},
			DispatchFunc: func(ctx context.Context, r string, out io.Writer) error {
				return orchestrator.RunCoordinated(ctx, orchestrator.CoordinatedOptions{
					Root:  r,
					Store: st,
				})
			},
		}
	}

	// The daemon merges and dispatches continuously: one Hub links its merger, its dispatcher, the
	// cycle and the portal, so a ready change is merged and an eligible item starts at once.
	buildDaemonOptions := func(st *store.Store) cycle.DaemonOptions {
		hub := orchestrator.NewHub()
		return cycle.DaemonOptions{
			CycleOpts: buildCycleOptions(st),
			StartWebFunc: func(ctx context.Context, r string, st *store.Store, bind string, port int, portExplicit bool) (string, func(context.Context) error, error) {
				srv, err := portal.New(r, st, portalKit(st), portal.WithHub(hub))
				if err != nil {
					return "", nil, err
				}
				return srv.StartBackground(ctx, bind, port, portExplicit)
			},
			ServeMergerFunc: func(ctx context.Context, r string, out io.Writer) error {
				m, err := orchestrator.NewMerger(orchestrator.MergerOptions{Root: r, Store: st, Hub: hub, Out: out})
				if err != nil {
					return err
				}
				defer m.Close()
				return m.Serve(ctx)
			},
			ServeDispatcherFunc: func(ctx context.Context, r string, out io.Writer) error {
				d, err := orchestrator.NewDispatcher(orchestrator.DispatcherOptions{Root: r, Store: st, Hub: hub, Out: out})
				if err != nil {
					return err
				}
				defer d.Close()
				return d.Serve(ctx)
			},
			Notifier: hub,
			// What each pass of the dispatcher left tells the cycle loop when the pipeline has run
			// out of work, so that the next cycle can start before its tick.
			DispatchLoadFunc: func() (cycle.DispatchReport, bool) {
				load, ok := hub.DispatchLoad()
				return cycle.DispatchReport(load), ok
			},
			DispatchSignal: hub.Dispatched(),
		}
	}

	rootCmd.RunE = func(cmd *cobra.Command, args []string) error {
		if rootDaemon {
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "daemon: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			dOpts := buildDaemonOptions(st)
			dOpts.Detach = true
			dOpts.Web = rootWeb
			dOpts.WebSet = cmd.Flags().Changed("web")
			dOpts.Port = rootPort
			dOpts.PortExplicit = cmd.Flags().Changed("port")
			dOpts.Bind = rootBind
			dOpts.Interval = rootInterval
			dOpts.NoInitialCycle = rootNoInitialCycle
			return wrapCode(cycle.StartDaemon(ctx, root, st, dOpts, stdout, stderr))
		}
		if rootWeb {
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "portal: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			var pArgs []string
			if rootPort > 0 {
				pArgs = append(pArgs, "--port", strconv.Itoa(rootPort))
			}
			if rootBind != "" {
				pArgs = append(pArgs, "--bind", rootBind)
			}
			return wrapCode(portal.RunPortalCLI(ctx, root, st, portalKit(st), pArgs, stdout, stderr))
		}
		return cmd.Help()
	}

	// 1. init / install
	initCmd := &cobra.Command{
		Use:                "init [app-dir] --agents LIST",
		Aliases:            []string{"install"},
		Short:              "Initialize the OpenSpec kit in an application umbrella",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			cwd, _ := os.Getwd()
			return wrapCode(cycle.RunKitCLI(ctx, cwd, append([]string{"init"}, args...), stdout, stderr))
		},
	}

	// 2. update / upgrade
	updateCmd := &cobra.Command{
		Use:                "update [app-dir]",
		Aliases:            []string{"upgrade"},
		Short:              "Upgrade the OpenSpec kit skills and templates in an application",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			cwd, _ := os.Getwd()
			return wrapCode(cycle.RunKitCLI(ctx, cwd, append([]string{"upgrade"}, args...), stdout, stderr))
		},
	}

	// 3. self-upgrade / self-update
	selfUpgradeCmd := &cobra.Command{
		Use:                "self-upgrade [tag-or-branch]",
		Aliases:            []string{"self-update"},
		Short:              "Upgrade the foreman-kit binary",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			cwd, _ := os.Getwd()
			return wrapCode(cycle.RunKitCLI(ctx, cwd, append([]string{"self-upgrade"}, args...), stdout, stderr))
		},
	}

	// 4. verify
	verifyCmd := &cobra.Command{
		Use:                "verify [app-dir]",
		Short:              "Verify installed kit files against .foreman-kit/kit.lock",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			cwd, _ := os.Getwd()
			return wrapCode(cycle.RunKitCLI(ctx, cwd, append([]string{"verify"}, args...), stdout, stderr))
		},
	}

	// 5. cycle
	cycleCmd := &cobra.Command{
		Use:                "cycle [--detach] [--strategy SLUG]",
		Short:              "Run one scheduled autonomous cycle",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "cycle: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			opts := cycle.CycleOptions{
				PreflightFunc: func(ctx context.Context, r string, out io.Writer) error {
					code := proc.RunPreflightCLI(ctx, r, st, nil, out, out)
					if code != 0 {
						return fmt.Errorf("preflight exited %d", code)
					}
					return nil
				},
				MergeFunc: func(ctx context.Context, r string, out io.Writer) error {
					m, err := orchestrator.NewMerger(orchestrator.MergerOptions{
						Root:  r,
						Store: st,
					})
					if err != nil {
						return err
					}
					defer m.Close()
					return m.Run(ctx)
				},
				AgentExecFunc: func(ctx context.Context, workdir, promptFile, engine string, timeout time.Duration, out io.Writer) error {
					res, err := proc.AgentExec(ctx, proc.AgentExecOptions{
						Workdir:        workdir,
						PromptFile:     promptFile,
						Engines:        engine,
						Slot:           98,
						TimeoutSeconds: int(timeout.Seconds()),
						Store:          st,
						Stdout:         out,
						Stderr:         out,
					})
					if res != nil && res.ExitCode != 0 {
						return fmt.Errorf("agent_exec exited %d", res.ExitCode)
					}
					return err
				},
				DispatchFunc: func(ctx context.Context, r string, out io.Writer) error {
					return orchestrator.RunCoordinated(ctx, orchestrator.CoordinatedOptions{
						Root:  r,
						Store: st,
					})
				},
			}
			return wrapCode(cycle.RunCycleCLI(ctx, root, st, opts, args, stdout, stderr))
		},
	}

	// 6. visual-qa / visual_qa
	visualQACmd := &cobra.Command{
		Use:                "visual-qa <begin|end|status|request|done|enqueue-integrated>",
		Aliases:            []string{"visual_qa"},
		Short:              "Manage the visual QA session and queue",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "visual-qa: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			return wrapCode(cycle.RunVisualQACLI(ctx, root, st, args, stdout, stderr))
		},
	}

	// 7. gates
	gatesCmd := &cobra.Command{
		Use:                "gates [--all] [--regression] [--base REF] [--repo NAME]",
		Short:              "Run diff-aware quality gates across child repositories",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "gates: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			return wrapCode(proc.RunGatesCLIWith(ctx, root, st, args, stdout, stderr, orchestrator.GateAcceptor(st, root)))
		},
	}

	// 8. exec / repo-exec
	execCmd := &cobra.Command{
		Use:                "exec <repo> <command> [args...]",
		Aliases:            []string{"repo-exec"},
		Short:              "Run a command in a child repository's environment",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) && len(args) == 1 {
				return cmd.Help()
			}
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "exec: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			return wrapCode(proc.RunRepoExecCLI(ctx, root, st, args, stdout, stderr))
		},
	}

	// 9. status
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show instant snapshot of guards, processes, queues, worktrees, and repositories",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "status: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			if err := cycle.Status(ctx, root, st, stdout); err != nil {
				fmt.Fprintf(stderr, "status: %v\n", err)
				return wrapCode(1)
			}
			return nil
		},
	}

	// 10. progress
	progressCmd := &cobra.Command{
		Use:                "progress [--since 24h|90m|7d|<time>] [--json]",
		Short:              "Report pipeline progress over a time period",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "progress: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			return wrapCode(cycle.RunProgressCLI(ctx, root, st, args, stdout, stderr))
		},
	}

	// 10b. base
	baseCmd := &cobra.Command{
		Use:                "base <status [--json]|accept <repo> [<sha>]>",
		Short:              "Show child repositories whose base fails on its own, or accept one so its held changes integrate",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "base: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			verify := func(ctx context.Context, repo, sha string) (string, error) {
				return orchestrator.VerifyBaseForAccept(ctx, root, st, repo, sha, stdout)
			}
			return wrapCode(cycle.RunBaseCLIWith(ctx, root, st, args, stdout, stderr, verify))
		},
	}

	// 11. config
	configCmd := &cobra.Command{
		Use:                "config <show|check|set|recommend> [args...]",
		Short:              "Show, validate, or edit project configuration files, or see what the records argue for changing",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			// The recommendations read what the kit recorded, so they need the state database;
			// the rest of `config` only reads and writes the files.
			if len(args) > 0 && args[0] == "recommend" {
				root, st, err := openWorkspaceStore()
				if err != nil {
					fmt.Fprintf(stderr, "config recommend: %v\n", err)
					return wrapCode(1)
				}
				defer st.Close()
				return wrapCode(cycle.RunRecommendCLI(ctx, root, st, args[1:], stdout, stderr))
			}
			root := config.AppRoot("")
			return wrapCode(config.RunConfigCLI(root, args, stdout, stderr))
		},
	}

	// 12. setup
	setupCmd := &cobra.Command{
		Use:                "setup <status|add-repo|repo> [args...]",
		Short:              "Inspect setup readiness or configure child repositories",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "setup: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			return wrapCode(cycle.RunSetupCLI(ctx, root, st, args, stdout, stderr))
		},
	}

	// 13. proposals
	proposalsCmd := &cobra.Command{
		Use:                "proposals <review|list|show|approve|reject|rebase|carry> [args...]",
		Short:              "Manage, review, and reconcile cycle proposals",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "proposals: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			judge, _ := evaluator.NewJudge(root, st)
			return wrapCode(cycle.RunProposalsCLI(ctx, root, st, judge, args, stdout, stderr))
		},
	}

	// 14. preflight / check-engines
	preflightCmd := &cobra.Command{
		Use:                "preflight [--services]",
		Aliases:            []string{"check-engines"},
		Short:              "Verify toolchain, kit drift, required services, the evaluator's canary, and AI engines",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "preflight: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			return wrapCode(proc.RunPreflightCLI(ctx, root, st, args, stdout, stderr))
		},
	}

	// 15. dispatch
	dispatchCmd := &cobra.Command{
		Use:                "dispatch [--workers N] [--wait [30m]] [--dry-run] [--json]",
		Short:              "Plan and build queued backlog items in parallel worktrees",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			if err := orchestrator.RunDispatchCLI(ctx, args, stdout, stderr); err != nil {
				fmt.Fprintf(stderr, "dispatch: %v\n", err)
				return wrapCode(1)
			}
			return nil
		},
	}

	// 16. merge
	mergeCmd := &cobra.Command{
		Use:                "merge [--watch [--interval 10s]]",
		Short:              "Integrate, verify, archive, and publish ready changes",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			if err := orchestrator.RunMergeCLI(ctx, args, stdout, stderr); err != nil {
				fmt.Fprintf(stderr, "merge: %v\n", err)
				return wrapCode(1)
			}
			return nil
		},
	}

	// 17. run (coordinated Dispatcher + Merger in-process)
	var runWorkers int
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run Dispatcher and Merger coordinated in a single process",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "run: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			if err := orchestrator.RunCoordinated(ctx, orchestrator.CoordinatedOptions{
				Root:       root,
				Store:      st,
				MaxWorkers: runWorkers,
			}); err != nil {
				fmt.Fprintf(stderr, "run: %v\n", err)
				return wrapCode(1)
			}
			return nil
		},
	}
	runCmd.Flags().IntVarP(&runWorkers, "workers", "w", 0, "Max concurrent workers")

	// 18. sync
	var syncPublish bool
	syncCmd := &cobra.Command{
		Use:   "sync [--publish]",
		Short: "Bring the main checkout and child repositories up to date with origin",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "sync: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			if err := cycle.SyncRepos(ctx, root, st, syncPublish, stdout); err != nil {
				fmt.Fprintf(stderr, "sync: %v\n", err)
				return wrapCode(1)
			}
			return nil
		},
	}
	syncCmd.Flags().BoolVar(&syncPublish, "publish", false, "Push ahead child commits and record updated pointers")

	// 19. completed-since
	completedSinceCmd := &cobra.Command{
		Use:   "completed-since <base-sha> [head-ref]",
		Short: "List integrated changes, child commits, and archived artifacts since a commit",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := config.AppRoot("")
			headRef := "HEAD"
			if len(args) > 1 {
				headRef = args[1]
			}
			if err := cycle.CompletedSince(ctx, root, args[0], headRef, stdout); err != nil {
				fmt.Fprintf(stderr, "completed-since: %v\n", err)
				return wrapCode(1)
			}
			return nil
		},
	}

	// 20. evaluation-report
	var reportSince, reportGate string
	var reportJSON bool
	evaluationReportCmd := &cobra.Command{
		Use:   "evaluation-report [--since PERIOD] [--gate NAME] [--json]",
		Short: "Report the evaluator's answers by gate alongside change outcomes",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "evaluation-report: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			sinceTime, err := cycle.ParseSince(reportSince, time.Now().UTC())
			if err != nil {
				fmt.Fprintf(stderr, "evaluation-report: %v\n", err)
				return wrapCode(2)
			}
			rep, err := evaluator.BuildReport(ctx, root, st, sinceTime, reportGate, nil)
			if err != nil {
				fmt.Fprintf(stderr, "evaluation-report: %v\n", err)
				return wrapCode(1)
			}
			if reportJSON {
				b, _ := json.MarshalIndent(rep, "", "  ")
				fmt.Fprintln(stdout, string(b))
			} else {
				fmt.Fprintln(stdout, evaluator.FormatReportText(rep))
			}
			return nil
		},
	}
	evaluationReportCmd.Flags().StringVar(&reportSince, "since", "30d", "Period to report")
	evaluationReportCmd.Flags().StringVar(&reportGate, "gate", "", "Filter to a single gate")
	evaluationReportCmd.Flags().BoolVar(&reportJSON, "json", false, "Machine-readable JSON output")

	// 21. evaluator-contract
	var contractLive bool
	evaluatorContractCmd := &cobra.Command{
		Use:   "evaluator-contract [--live]",
		Short: "Run the offline and optional live calibration checks of the evaluator's policy and questions",
		RunE: func(cmd *cobra.Command, args []string) error {
			root := config.AppRoot("")
			failures := evaluator.RunContract(ctx, root, contractLive, stdout)
			if failures > 0 {
				return wrapCode(1)
			}
			return nil
		},
	}
	evaluatorContractCmd.Flags().BoolVar(&contractLive, "live", false, "Run live calibration against TypeSafe AI API")

	// 22. evaluate (native subcommand for agents: noul, eval, validate)
	evaluateCmd := buildEvaluateSubcommands(ctx, stdout, stderr, wrapCode)

	// 23. prompt-report
	promptReportCmd := &cobra.Command{
		Use:                "prompt-report [--since PERIOD] [--by prompt|memory] [--json]",
		Short:              "Report how each version of prompts or memory performed",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "prompt_report: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			return wrapCode(config.RunPromptReportCLI(ctx, root, st, args, stdout, stderr))
		},
	}

	// 24. prompts
	promptsCmd := &cobra.Command{
		Use:                "prompts <check|render|version> [args...]",
		Short:              "Validate, render, or version agent prompt templates",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root := config.AppRoot("")
			return wrapCode(config.RunPromptsCLI(root, args, stdout, stderr))
		},
	}

	// 25. memory
	memoryCmd := &cobra.Command{
		Use:                "memory <status|fingerprint>",
		Short:              "Show each file of the memory (.agent-memory/) against its cap, or print the memory's fingerprint",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root := config.AppRoot("")
			return wrapCode(memory.RunCLI(root, args, stdout, stderr))
		},
	}

	// 26. agents
	agentsCmd := &cobra.Command{
		Use:                "agents <engine|list|selection|permissions|arrange|claim|release|in-use|args> [args...]",
		Short:              "Inspect and manage configured AI agent engines",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "agents: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			return wrapCode(config.RunAgentsCLI(ctx, root, st, args, stdout, stderr))
		},
	}

	// 27. settings
	settingsCmd := &cobra.Command{
		Use:                "settings get <key>",
		Short:              "Read a setting of .foreman-kit/pipeline.yaml, written as its file writes it",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root := config.AppRoot("")
			return wrapCode(config.RunSettingsCLI(root, args, stdout, stderr))
		},
	}

	// 28. agent-exec
	agentExecCmd := &cobra.Command{
		Use:                "agent-exec <workdir> <prompt-file> [engine[,engine...]]",
		Short:              "Execute an AI agent CLI with bounded timeout, idle watchdog, and fallback",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			_, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "agent-exec: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			return wrapCode(proc.RunAgentExecCLI(ctx, st, args, stdout, stderr))
		},
	}

	// 29. cycle-prompt
	cyclePromptCmd := &cobra.Command{
		Use:                "cycle-prompt <output-path> [--dispatch-running PID | --dispatch-continuous [PID]] [--strategy SLUG] [--queue-open N]",
		Short:              "Render the scheduled cycle judgment prompt",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root := config.AppRoot("")
			return wrapCode(cycle.RunCyclePromptCLI(ctx, root, args, stdout, stderr))
		},
	}

	// 30. portal
	portalCmd := &cobra.Command{
		Use:                "portal [--port 4567] [--bind 127.0.0.1]",
		Short:              "Start the local operator web portal with SSE streaming",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "portal: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			return wrapCode(portal.RunPortalCLI(ctx, root, st, portalKit(st), args, stdout, stderr))
		},
	}

	// 31. db (db path)
	dbCmd := &cobra.Command{
		Use:   "db",
		Short: "Inspect workspace SQLite state database",
	}
	dbPathCmd := &cobra.Command{
		Use:   "path [dir]",
		Short: "Print the resolved SQLite state database path (~/.foreman-kit/state/<workspace_id>.db)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := ""
			if len(args) == 1 {
				dir = args[0]
			}
			p, _, _, err := store.ResolveDBPath(dir)
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, p)
			return nil
		},
	}
	dbCmd.AddCommand(dbPathCmd)

	// 32. sim (repeatable multi-scenario simulation stack with configurable fake agent binaries)
	simCmd := &cobra.Command{
		Use:                "sim <init|reset|run|status|agents> [stack-dir] [scenario]",
		Short:              "Initialize, reset, and run repeatable simulated agent scenarios",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return wrapCode(sim.RunSimCLI(ctx, args, stdout, stderr))
		},
	}

	// 33. migrate-from-files
	migrateFromFilesCmd := &cobra.Command{
		Use:   "migrate-from-files [<app-dir>]",
		Short: "Import legacy log/ and config/ files into SQLite idempotently",
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) > 0 {
				target = args[0]
			}
			st, err := store.Open(target)
			if err != nil {
				fmt.Fprintf(stderr, "migrate-from-files: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			if _, err := st.MigrateFromFiles(ctx, target, stdout); err != nil {
				fmt.Fprintf(stderr, "migrate-from-files: %v\n", err)
				return wrapCode(1)
			}
			return nil
		},
	}

	// 34. daemon
	daemonCmd := &cobra.Command{
		Use:                "daemon [start|run|stop|status|restart] [-d|--daemon] [-w|--web] [--port 4567] [--bind 127.0.0.1] [--interval 60m]",
		Short:              "Manage the per-project background daemon",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			root, st, err := openWorkspaceStore()
			if err != nil {
				fmt.Fprintf(stderr, "daemon: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()
			return wrapCode(cycle.RunDaemonCLI(ctx, root, st, buildDaemonOptions(st), args, stdout, stderr))
		},
	}

	// 35. version
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print foreman-kit version",
		Run: func(cmd *cobra.Command, args []string) {
			if assets.BuiltAt != "" {
				fmt.Fprintf(stdout, "foreman-kit v%s (%s, built %s)\n", assets.Version, assets.SourceCommit, assets.BuiltAt)
				return
			}
			fmt.Fprintf(stdout, "foreman-kit v%s (%s)\n", assets.Version, assets.SourceCommit)
		},
	}

	// 36. unblock
	unblockCmd := &cobra.Command{
		Use:   "unblock <change> [dir]",
		Short: "Clear quarantine and retry state for a blocked change",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			change := strings.TrimSpace(args[0])
			if !cycle.ChangeNameRe.MatchString(change) {
				fmt.Fprintf(stderr, "unblock: invalid change name %q\n", change)
				return wrapCode(2)
			}
			dirArg := ""
			if len(args) > 1 {
				dirArg = args[1]
			}
			root, st, err := openTargetStore(dirArg)
			if err != nil {
				fmt.Fprintf(stderr, "unblock: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()

			// An operator lifts the block and the change's retries start over; the blocks it had stay
			// on record, resolved. A risk approval a person already gave is kept.
			q, _ := st.GetQuarantine(ctx, change)
			if err := st.ResolveQuarantine(ctx, change, store.QuarantineResolution{
				Resolution:   store.BlockUnblocked,
				By:           "cli",
				Forget:       true,
				KeepApproval: q != nil && q.ApprovedRisk,
			}); err != nil {
				fmt.Fprintf(stderr, "unblock: %v\n", err)
				return wrapCode(1)
			}
			_ = os.Remove(filepath.Join(root, "log", "blocked", change+".json"))
			_ = os.Remove(needsHumanFile(root, change))
			fmt.Fprintf(stdout, "unblocked: %s\n", change)
			return nil
		},
	}

	// 37. approve
	approveCmd := &cobra.Command{
		Use:   "approve <change> [dir]",
		Short: "Approve a risk_review, concurrent_work_loss or semantic_conflict change in SQLite without touching log/approved/",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			change := strings.TrimSpace(args[0])
			if !cycle.ChangeNameRe.MatchString(change) {
				fmt.Fprintf(stderr, "approve: invalid change name %q\n", change)
				return wrapCode(2)
			}
			dirArg := ""
			if len(args) > 1 {
				dirArg = args[1]
			}
			root, st, err := openTargetStore(dirArg)
			if err != nil {
				fmt.Fprintf(stderr, "approve: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()

			q, _ := st.GetQuarantine(ctx, change)
			if err := st.ApproveChangeRiskBy(ctx, change, "cli"); err != nil {
				fmt.Fprintf(stderr, "approve: %v\n", err)
				return wrapCode(1)
			}
			if q != nil && (q.Reason == "risk_review" || q.Reason == "concurrent_work_loss" || q.Reason == "semantic_conflict") {
				if err := st.ClearQuarantine(ctx, change, false); err != nil {
					fmt.Fprintf(stderr, "approve: %v\n", err)
					return wrapCode(1)
				}
			}
			_ = os.Remove(filepath.Join(root, "log", "blocked", change+".json"))
			_ = os.Remove(needsHumanFile(root, change))
			fmt.Fprintf(stdout, "approved: %s\n", change)
			return nil
		},
	}

	// 38. abandon
	abandonCmd := &cobra.Command{
		Use:   "abandon <change> [dir]",
		Short: "Remove a change from the backlog and mark it abandoned in SQLite",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			change := strings.TrimSpace(args[0])
			if !cycle.ChangeNameRe.MatchString(change) {
				fmt.Fprintf(stderr, "abandon: invalid change name %q\n", change)
				return wrapCode(2)
			}
			dirArg := ""
			if len(args) > 1 {
				dirArg = args[1]
			}
			root, st, err := openTargetStore(dirArg)
			if err != nil {
				fmt.Fprintf(stderr, "abandon: %v\n", err)
				return wrapCode(1)
			}
			defer st.Close()

			backlogPath := filepath.Join(root, cycle.BacklogFile)
			if items, err := cycle.ReadJSONList(backlogPath); err == nil && len(items) > 0 {
				var kept []map[string]any
				changed := false
				for _, it := range items {
					if strings.TrimSpace(fmt.Sprint(it["change_name"])) == change {
						changed = true
						continue
					}
					kept = append(kept, it)
				}
				if changed {
					if err := cycle.WriteJSONList(backlogPath, kept); err != nil {
						fmt.Fprintf(stderr, "abandon: %v\n", err)
						return wrapCode(1)
					}
				}
			}

			// Abandoned: out of the merge queue, its quarantine closed, and discarded in the registry.
			_ = st.CloseReady(ctx, change, store.ManifestRemoved)
			_ = st.ResolveQuarantine(ctx, change, store.QuarantineResolution{Resolution: store.BlockAbandoned, By: "cli", Forget: true})
			_ = st.MarkChangeDiscarded(ctx, change)
			_ = os.Remove(filepath.Join(root, "log", "blocked", change+".json"))
			_ = os.Remove(needsHumanFile(root, change))
			fmt.Fprintf(stdout, "abandoned: %s\n", change)
			return nil
		},
	}

	rootCmd.AddCommand(
		initCmd,
		updateCmd,
		selfUpgradeCmd,
		verifyCmd,
		cycleCmd,
		visualQACmd,
		gatesCmd,
		execCmd,
		statusCmd,
		progressCmd,
		baseCmd,
		configCmd,
		setupCmd,
		proposalsCmd,
		preflightCmd,
		dispatchCmd,
		mergeCmd,
		runCmd,
		syncCmd,
		completedSinceCmd,
		evaluationReportCmd,
		evaluatorContractCmd,
		evaluateCmd,
		promptReportCmd,
		promptsCmd,
		memoryCmd,
		agentsCmd,
		settingsCmd,
		agentExecCmd,
		cyclePromptCmd,
		portalCmd,
		dbCmd,
		simCmd,
		migrateFromFilesCmd,
		daemonCmd,
		versionCmd,
		unblockCmd,
		approveCmd,
		abandonCmd,
	)

	return rootCmd
}

func buildEvaluateSubcommands(ctx context.Context, stdout, stderr io.Writer, wrapCode func(int) error) *cobra.Command {
	evaluateCmd := &cobra.Command{
		Use:   "evaluate",
		Short: "Ask the evaluator calibrated questions, natively through go-jev",
	}

	var fileFlag, stateFileFlag, modelFlag string
	var dryRunFlag bool
	var assertsFlag []string
	evalCmd := &cobra.Command{
		Use:   "eval",
		Short: "Evaluate a question YAML file against a state",
		RunE: func(cmd *cobra.Command, args []string) error {
			if m := strings.TrimSpace(modelFlag); m != "" {
				// --model is the model of this one evaluation: what --set evaluator.model gives a run.
				restore, err := settings.GiveForThisRun(map[settings.Key]string{settings.EvaluatorModel: m})
				if err != nil {
					return err
				}
				defer restore()
			}
			root := config.AppRoot("")
			st, _ := store.Open(root)
			if st != nil {
				defer st.Close()
			}
			judge, err := evaluator.NewJudge(root, st)
			if err != nil {
				fmt.Fprintf(stderr, "evaluate eval: %v\n", err)
				return wrapCode(2)
			}
			var state any
			if stateFileFlag != "" {
				raw, err := os.ReadFile(stateFileFlag)
				if err != nil {
					fmt.Fprintf(stderr, "evaluate eval: %v\n", err)
					return wrapCode(2)
				}
				var parsed any
				if json.Unmarshal(raw, &parsed) == nil {
					state = parsed
				} else {
					state = string(raw)
				}
			}
			res, code, err := judge.EvaluateTemplate(ctx, fileFlag, state, assertsFlag, dryRunFlag)
			if err != nil {
				fmt.Fprintf(stderr, "evaluate eval: %v\n", err)
				return wrapCode(code)
			}
			b, _ := json.MarshalIndent(res, "", "  ")
			fmt.Fprintln(stdout, string(b))
			return wrapCode(code)
		},
	}
	evalCmd.Flags().StringVarP(&fileFlag, "file", "f", "", "Question template YAML file")
	evalCmd.Flags().StringVar(&stateFileFlag, "state-file", "", "State file path")
	evalCmd.Flags().StringVar(&modelFlag, "model", "", "Model override")
	evalCmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "Validate questions and asserts without calling API")
	evalCmd.Flags().StringArrayVar(&assertsFlag, "assert", nil, "Condition to assert (e.g. 'q >= 0.6')")

	var valFile string
	var strictFlag bool
	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate a question template file",
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = strictFlag
			root := config.AppRoot("")
			_, _, err := evaluator.LoadTemplate(root, valFile)
			if err != nil {
				fmt.Fprintf(stderr, "evaluate validate: %v\n", err)
				return wrapCode(2)
			}
			fmt.Fprintln(stdout, `{"valid":true}`)
			return nil
		},
	}
	validateCmd.Flags().StringVarP(&valFile, "file", "f", "", "Question template YAML file")
	validateCmd.Flags().BoolVar(&strictFlag, "strict", true, "Strict validation")

	var noulStateFile, noulModelFlag string
	noulCmd := &cobra.Command{
		Use:   "noul <question>",
		Short: "Ask the evaluator a single yes/no (noul) question about a state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if m := strings.TrimSpace(noulModelFlag); m != "" {
				restore, err := settings.GiveForThisRun(map[settings.Key]string{settings.EvaluatorModel: m})
				if err != nil {
					return err
				}
				defer restore()
			}
			var stateBytes []byte
			var err error
			if noulStateFile != "" {
				stateBytes, err = os.ReadFile(noulStateFile)
			} else {
				var buf bytes.Buffer
				_, err = io.Copy(&buf, os.Stdin)
				stateBytes = buf.Bytes()
			}
			if err != nil {
				fmt.Fprintf(stderr, "evaluate noul: %v\n", err)
				return wrapCode(2)
			}
			root := config.AppRoot("")
			judge, err := evaluator.NewJudge(root, nil)
			if err != nil {
				fmt.Fprintf(stderr, "evaluate noul: %v\n", err)
				return wrapCode(2)
			}
			tmpTpl := fmt.Sprintf("questions:\n  answer:\n    type: noul\n    instructions: %q\n", args[0])
			tmpFile, err := os.CreateTemp("", "evaluate-noul-*.yaml")
			if err != nil {
				return err
			}
			defer os.Remove(tmpFile.Name())
			_, _ = tmpFile.WriteString(tmpTpl)
			_ = tmpFile.Close()

			res, code, err := judge.EvaluateTemplate(ctx, tmpFile.Name(), string(stateBytes), nil, false)
			if err != nil {
				fmt.Fprintf(stderr, "evaluate noul: %v\n", err)
				return wrapCode(code)
			}
			b, _ := json.MarshalIndent(res, "", "  ")
			fmt.Fprintln(stdout, string(b))
			return nil
		},
	}
	noulCmd.Flags().StringVar(&noulStateFile, "state-file", "", "State file path")
	noulCmd.Flags().StringVar(&noulModelFlag, "model", "", "Model override")

	evaluateCmd.AddCommand(evalCmd, validateCmd, noulCmd)
	return evaluateCmd
}

// Run executes the Cobra CLI with args and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if err := settings.RunOverridesProblem(); err != nil {
		fmt.Fprintf(stderr, "foreman-kit: %v\n", err)
		return 2
	}
	args, err := takeRunOverrides(args)
	if err != nil {
		fmt.Fprintf(stderr, "foreman-kit: %v\n", err)
		return 2
	}
	// What only the application can say of a value: an agent it does not list, a slot that is
	// taken. It is asked of the application this command runs in, before anything starts.
	if err := config.CheckRunOverrides(config.AppRoot("")); err != nil {
		fmt.Fprintf(stderr, "foreman-kit: %v\n", err)
		return 2
	}
	cmd := NewRootCmd(ctx, stdout, stderr)
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
		var ec exitCodeError
		if errors.As(err, &ec) {
			return ec.code
		}
		fmt.Fprintf(stderr, "foreman-kit: %v\n", err)
		return 2
	}
	return 0
}

// takeRunOverrides takes the `--set <file>.<key>=<value>` options written before the command (or
// among the options of `foreman-kit -d`), checks each one against the catalog, and hands them to
// this process and to everything it starts through settings.RunOverridesEnv. It returns the other
// arguments. The values hold for this run alone: no file is written.
//
// An option after the command is the command's own (`foreman-kit exec backend make --set x=y`
// passes --set to make), so the reading stops at the first word that is not an option.
func takeRunOverrides(args []string) ([]string, error) {
	var given []string
	rest := make([]string, 0, len(args))
	i := 0
	for ; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--set":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--set takes <file>.<key>=<value>, for example --set %s=3", settings.Workers)
			}
			given = append(given, args[i+1])
			i++
			continue
		case strings.HasPrefix(arg, "--set="):
			given = append(given, strings.TrimPrefix(arg, "--set="))
			continue
		}
		if arg == "--" || !strings.HasPrefix(arg, "-") {
			// The value of an option of the daemon (--port 4567) is passed over: --set may follow it.
			if i > 0 && takesValue(args[i-1]) {
				rest = append(rest, arg)
				continue
			}
			break
		}
		rest = append(rest, arg)
	}
	rest = append(rest, args[i:]...)
	if len(given) == 0 {
		return rest, nil
	}
	values := settings.DecodeOverrides(os.Getenv(settings.RunOverridesEnv))
	if values == nil {
		values = map[settings.Key]string{}
	}
	for _, arg := range given {
		key, text, err := settings.ParseOverride(arg)
		if err != nil {
			return nil, err
		}
		values[key] = text
	}
	if err := os.Setenv(settings.RunOverridesEnv, settings.EncodeOverrides(values)); err != nil {
		return nil, err
	}
	return rest, nil
}

// takesValue reports whether arg is an option of the root command whose value is the next word.
func takesValue(arg string) bool {
	switch arg {
	case "--port", "--bind", "--interval":
		return true
	}
	return false
}

func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

var _ = strings.TrimSpace

// needsHumanFile is where a worker leaves its question about change for a human: in the change's
// worktree, under the worktree base the dispatcher and the portal use (repository.worktrees_dir of
// .foreman-kit/pipeline.yaml). The dispatcher blocks the change again for as long as
// the file exists, so unblock, approve and abandon must remove the one it reads, also when the
// worktrees live elsewhere or the command runs from inside a worktree.
func needsHumanFile(root, change string) string {
	mainRoot := store.MainRoot(root)
	base := settings.WorktreesBase(mainRoot)
	return filepath.Join(base, "worker-"+change, "openspec", "changes", change, ".needs-human")
}

// portalKit returns what the portal starts processes with: this executable's own operations.
func portalKit(st *store.Store) portal.Kit {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	return portal.NewKit(st, exe)
}

// shutdownSignals lists the signals that cancel the root context, so agent and gate process groups
// (started with Setpgid, so they never receive terminal signals themselves) are stopped before the process
// exits. SIGHUP covers a closed terminal or a dropped SSH session; it is left alone when the process
// inherited it as ignored (nohup), because signal.Notify would otherwise re-enable it.
func shutdownSignals() []os.Signal {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return sigs
}

// surviveBrokenPipe keeps a write to a closed stdout or stderr pipe from killing the process. Go exits
// with SIGPIPE on such a write unless the program asked to be notified of that signal. In
// `foreman-kit run | tee run.log`, Ctrl-C stops tee first: the next line this process printed killed
// it in the middle of stopping its agents, whose process groups (never signaled by the terminal) kept
// running with nobody left to stop them. With the signal claimed the write only fails, and the command
// goes on to its end or to the shutdown the same Ctrl-C asked for.
//
// The signal is claimed, not ignored: an ignored signal stays ignored in every command this process
// starts, and a shell pipeline in a gate relies on SIGPIPE to end. It is not a shutdown signal either:
// the kernel also raises it for a portal client that went away. When the process inherited it as
// ignored a write already fails without killing, and claiming it would change what children inherit.
func surviveBrokenPipe() {
	if signal.Ignored(syscall.SIGPIPE) {
		return
	}
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
}

// adoptFormerNames gives each FOREMANKIT_<name> the value of OPENSPEC_<name> when only the second is
// set: the variables of the kit were called that before it was called Foreman, and a script or a
// shell written then (an env.sh that sets OPENSPEC_STATE_DIR) keeps working. A variable a former kit
// read and this one does not is still refused by name (settings.FormerVariables).
func adoptFormerNames() {
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "OPENSPEC_") {
			continue
		}
		current := "FOREMANKIT_" + strings.TrimPrefix(name, "OPENSPEC_")
		if name == "OPENSPEC_KIT_HOME" {
			current = "FOREMANKIT_HOME"
		}
		if _, set := os.LookupEnv(current); !set {
			_ = os.Setenv(current, value)
		}
	}
}
