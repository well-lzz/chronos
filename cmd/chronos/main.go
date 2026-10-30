// Command chronos runs one Assessment-mode engagement: a single multi-target
// mission with a shared, persisted blackboard and real-traffic evidence. No
// flag oracle — findings are the product, written to a local report.
//
//	chronos --engagement path.json
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/engagement"
	"github.com/well-lzz/chronos/internal/engrun"
	"github.com/well-lzz/chronos/internal/guard"
	"github.com/well-lzz/chronos/internal/llm"
	"github.com/well-lzz/chronos/internal/llmconfig"
	"github.com/well-lzz/chronos/internal/llmrec"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/notify"
	"github.com/well-lzz/chronos/internal/platform"
	"github.com/well-lzz/chronos/internal/report"
	"github.com/well-lzz/chronos/internal/runhistory"
	"github.com/well-lzz/chronos/internal/scheduler"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/swarm"
	"github.com/well-lzz/chronos/internal/traffic"
	"github.com/well-lzz/chronos/internal/web"
)

type modelList []string

func (m *modelList) String() string { return strings.Join(*m, ",") }
func (m *modelList) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func main() {
	var (
		engPath    string
		modelsFile string
		models     modelList
		allowExec  bool
		noExec     bool
		timeLimit  int
		webUI      bool
		webHost    string
		webPort    int
		maxCh      int
		preflight  bool
	)
	flag.StringVar(&engPath, "engagement", "", "run an Assessment engagement from a scope config (JSON)")
	flag.Var(&models, "model", "model spec, repeatable (default deepseek/deepseek-chat)")
	flag.StringVar(&modelsFile, "models-file", "", "JSON file of operator-selected OpenAI-compatible executor models")
	flag.BoolVar(&allowExec, "allow-exec", false, "allow model-generated Python on this host (unsafe; opt-in)")
	flag.BoolVar(&noExec, "no-exec", false, "disable the run_python tool (deprecated; execution is disabled by default)")
	flag.IntVar(&timeLimit, "time-limit", 0, "per-mission wall-clock seconds (default from config)")
	flag.BoolVar(&webUI, "web", false, "launch the web console alongside the scheduler")
	flag.StringVar(&webHost, "web-host", "127.0.0.1", "web console host")
	flag.IntVar(&webPort, "web-port", 8000, "web console port")
	flag.IntVar(&maxCh, "max-challenges", 0, "concurrent challenge window (default from config, hard cap 3)")
	flag.BoolVar(&preflight, "preflight", false, "validate runtime configuration without contacting the platform or model")
	flag.Parse()

	// Convenience: load KEY=VALUE pairs from a .env in the working directory (and
	// next to the binary) before reading config, so the operator doesn't have to
	// export the API key by hand. Real environment variables always win.
	loadDotEnv(".env")
	if exe, err := os.Executable(); err == nil {
		loadDotEnv(filepath.Join(filepath.Dir(exe), ".env"))
	}

	cfg := config.Load()
	if allowExec {
		cfg.AllowLocalExec = true
	}
	if noExec {
		cfg.AllowLocalExec = false
	}
	if timeLimit > 0 {
		cfg.ChallengeTimeLimitS = timeLimit
	}
	if maxCh > 0 {
		cfg.MaxConcurrent = maxCh
	}
	if cfg.MaxConcurrent > 3 {
		cfg.MaxConcurrent = 3
	}
	if err := guard.Shared(cfg.StateDir, cfg.ToolRulesFile).LoadError(); err != nil {
		fmt.Fprintln(os.Stderr, "tool rules:", err)
		os.Exit(2)
	}
	if _, err := notify.Shared(cfg.StateDir, cfg.WebhookURL); err != nil {
		fmt.Fprintln(os.Stderr, "webhook:", err)
		os.Exit(2)
	}

	// Operator-editable model profile (configured from the console, persisted to
	// the state dir). When set and no --model flag was given, it drives the run;
	// any OpenAI-compatible provider works.
	llmStore := llmconfig.Load(cfg.StateDir)
	if len(cfg.DeepSeekKeys) > 0 {
		llmStore.UseFallback(llmconfig.Profile{
			Provider: "deepseek", Model: "deepseek-chat",
			BaseURL: cfg.DeepSeekBaseURL, APIKey: cfg.DeepSeekKeys[0],
		})
	}

	if modelsFile != "" && len(models) > 0 {
		fmt.Fprintln(os.Stderr, "--models-file and --model cannot be used together")
		os.Exit(2)
	}
	useConsoleProfile := modelsFile == "" && len(models) == 0
	var modelProfiles map[string]llmconfig.Profile
	specs := []string(models)
	if modelsFile != "" {
		var err error
		specs, modelProfiles, err = llmconfig.LoadModelsFile(modelsFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "models file:", err)
			os.Exit(2)
		}
	}
	if len(specs) == 0 {
		if p := llmStore.Get(); p.Active() {
			specs = []string{p.Label()}
		} else {
			specs = []string{"deepseek/deepseek-chat"}
		}
	}
	if preflight {
		if err := runPreflight(cfg, specs, llmStore, modelProfiles, useConsoleProfile); err != nil {
			fmt.Fprintln(os.Stderr, "preflight failed:", err)
			os.Exit(1)
		}
		return
	}

	// Engagement mode: one authorized assessment, no platform/scheduler.
	if engPath != "" {
		if err := runEngagement(cfg, engPath, specs, llmStore, modelProfiles, useConsoleProfile, webUI, webHost, webPort, timeLimit); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	// Otherwise run the mechanical scheduler against the TSec platform.
	if err := runScheduler(cfg, specs, llmStore, modelProfiles, useConsoleProfile, webUI, webHost, webPort); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runPreflight(cfg config.Settings, specs []string, llmStore *llmconfig.Store, profiles map[string]llmconfig.Profile, useConsoleProfile bool) error {
	var failures []string
	base, err := url.Parse(cfg.PlatformBase())
	if err != nil || base.Hostname() == "" || (base.Scheme != "http" && base.Scheme != "https") {
		failures = append(failures, "BENCHMARK_BASE_URL/PLATFORM_URL must be an absolute HTTP(S) URL")
	}
	if strings.TrimSpace(cfg.BenchmarkToken) == "" {
		failures = append(failures, "BENCHMARK_TOKEN is not configured")
	}
	if cfg.MaxConcurrent < 1 || cfg.MaxConcurrent > 3 {
		failures = append(failures, "MAX_CONCURRENT_CHALLENGES must be between 1 and 3")
	}

	type modelCheck struct {
		label, base string
		keys        int
	}
	var modelChecks []modelCheck
	addBackupChecks := func(label string, profile llmconfig.Profile) {
		for i, backup := range profile.Fallbacks {
			base := backup.BaseURL
			if cfg.TSecBenchHosted {
				base = config.TSecBenchGatewayURL(base)
			}
			keys := 0
			if strings.TrimSpace(backup.APIKey) != "" {
				keys = 1
			}
			modelChecks = append(modelChecks, modelCheck{fmt.Sprintf("%s fallback %d", label, i+1), base, keys})
		}
	}
	if len(profiles) > 0 {
		for _, spec := range specs {
			profile := profiles[spec]
			base := profile.BaseURL
			if cfg.TSecBenchHosted {
				base = config.TSecBenchGatewayURL(base)
			}
			modelChecks = append(modelChecks, modelCheck{spec, base, 1})
			addBackupChecks(spec, profile)
		}
	} else if useConsoleProfile && llmStore != nil && llmStore.Active() {
		profile := llmStore.Get()
		modelBase := profile.BaseURL
		if cfg.TSecBenchHosted {
			modelBase = config.TSecBenchGatewayURL(modelBase)
		}
		// Reflect the real rotation set: a DeepSeek profile pools the env keys too.
		modelChecks = append(modelChecks, modelCheck{profile.Label(), modelBase, len(llm.ProfileKeys(profile, cfg))})
		addBackupChecks(profile.Label(), profile)
	} else {
		for _, spec := range specs {
			provider := strings.SplitN(spec, "/", 2)[0]
			check := modelCheck{label: spec}
			switch provider {
			case "", "deepseek":
				check.base, check.keys = cfg.DeepSeekBaseURL, len(cfg.DeepSeekKeys)
				if check.keys == 0 && strings.TrimSpace(cfg.DeepSeekKey) != "" {
					check.keys = 1
				}
			case "qwen":
				check.base = cfg.QwenBaseURL
				if strings.TrimSpace(cfg.QwenKey) != "" {
					check.keys = 1
				}
			case "glm":
				check.base = cfg.GLMBaseURL
				if strings.TrimSpace(cfg.GLMKey) != "" {
					check.keys = 1
				}
			default:
				failures = append(failures, fmt.Sprintf("%s: unsupported environment model provider %q", spec, provider))
			}
			modelChecks = append(modelChecks, check)
		}
	}
	for _, check := range modelChecks {
		if check.keys == 0 {
			failures = append(failures, check.label+": API key is not configured")
		}
		if parsed, parseErr := url.Parse(check.base); parseErr != nil || parsed.Hostname() == "" {
			failures = append(failures, check.label+": base URL is invalid")
		}
		if cfg.TSecBenchHosted && !config.IsTSecBenchGatewayURL(check.base) {
			failures = append(failures, check.label+": endpoint is not routed through *.tsecbench.gw")
		}
	}

	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		failures = append(failures, "state directory cannot be created: "+err.Error())
	} else if f, err := os.CreateTemp(cfg.StateDir, ".preflight-*"); err != nil {
		failures = append(failures, "state directory is not writable: "+err.Error())
	} else {
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
	}
	if cfg.AllowLocalExec {
		if _, err := exec.LookPath("python3"); err != nil {
			failures = append(failures, "ALLOW_LOCAL_EXEC is enabled but python3 is unavailable")
		}
	}

	fmt.Println("Chronos preflight (offline; no evaluation was started)")
	fmt.Printf("  Platform: %s (%s)\n", cfg.PlatformMode, cfg.PlatformBase())
	fmt.Printf("  BENCHMARK_TOKEN: configured=%t\n", strings.TrimSpace(cfg.BenchmarkToken) != "")
	fmt.Printf("  Hosted routing: %t\n", cfg.TSecBenchHosted)
	for _, check := range modelChecks {
		fmt.Printf("  Model: %s (%s), keys configured=%d\n", check.label, check.base, check.keys)
	}
	fmt.Printf("  Concurrency: %d/3; total budget: %ds\n", cfg.MaxConcurrent, cfg.TotalBudgetSeconds)
	fmt.Printf("  Python tool: enabled=%t; state: %s\n", cfg.AllowLocalExec, cfg.StateDir)
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	fmt.Println("  Result: ready")
	return nil
}

// loadDotEnv reads simple KEY=VALUE lines from path and sets any that are not
// already present in the environment. Missing file is fine. Supports # comments,
// optional "export " prefixes, and single/double-quoted values.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		if key != "" {
			if _, ok := os.LookupEnv(key); !ok {
				_ = os.Setenv(key, val)
			}
		}
	}
}

func runScheduler(cfg config.Settings, specs []string, llmStore *llmconfig.Store, profiles map[string]llmconfig.Profile, useConsoleProfile bool, webUI bool, webHost string, webPort int) error {
	pf, err := platform.New(cfg.PlatformMode, cfg.PlatformBase(), cfg.BenchmarkToken, cfg.PlatformInsecureTLS)
	if err != nil {
		return err
	}
	sch := scheduler.New(pf, cfg, specs)
	sch.LLM = llmStore
	sch.ModelProfiles = profiles
	sch.UseConsoleProfile = useConsoleProfile
	history := &runhistory.Store{StateDir: cfg.StateDir}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("chronos scheduler — platform adapter %q (%s), models %s, window %d\n",
		cfg.PlatformMode, cfg.PlatformBase(), strings.Join(specs, ", "), cfg.MaxConcurrent)
	if len(profiles) == 0 && usesDeepSeek(specs) && len(cfg.DeepSeekKeys) > 1 {
		fmt.Printf("DeepSeek API key pool: %d keys (request-level round-robin; automatic failover enabled)\n", len(cfg.DeepSeekKeys))
	}
	if webUI {
		fmt.Printf("Web console → http://%s:%d (Ctrl+C to stop)\n", webHost, webPort)
		go func() {
			if err := web.ServeWithServices(ctx, sch, llmStore, web.Services{Recorder: llmrec.Shared(cfg.StateDir, cfg.LLMRecord), Guard: guard.Shared(cfg.StateDir, cfg.ToolRulesFile), History: history}, webHost, webPort); err != nil {
				fmt.Fprintln(os.Stderr, "web console error:", err)
			}
		}()
	}
	return sch.Run(ctx)
}

func usesDeepSeek(specs []string) bool {
	for _, spec := range specs {
		provider := strings.SplitN(strings.TrimSpace(spec), "/", 2)[0]
		if provider == "" || provider == "deepseek" {
			return true
		}
	}
	return false
}

func runEngagement(cfg config.Settings, path string, specs []string, llmStore *llmconfig.Store, profiles map[string]llmconfig.Profile, useConsoleProfile bool, webUI bool, webHost string, webPort, timeLimit int) error {
	eng, err := engagement.LoadDraft(path)
	if err != nil {
		return fmt.Errorf("engagement refused: %w", err)
	}
	meta := eng.ToMeta()
	sc := eng.Scope()
	runtimeID := eng.Name + "\x00" + eng.Goal
	previous, err := engagement.LoadRuntime(cfg.StateDir, runtimeID)
	if err != nil {
		return fmt.Errorf("load engagement runtime: %w", err)
	}
	scopeID := "engagement\x00" + runtimeID
	saved, runID, found, err := scope.LoadOperatorScopeRun(cfg.StateDir, scopeID)
	if err != nil {
		return fmt.Errorf("load operator scope: %w", err)
	}
	if found {
		sc = saved
		if runID == "" && !previous.Started.IsZero() {
			runID, err = scope.NewRunID()
			if err != nil {
				return err
			}
			if err := scope.SaveOperatorScopeRun(cfg.StateDir, scopeID, sc, runID); err != nil {
				return err
			}
		}
	} else if len(previous.Scope) > 0 {
		sc = scope.Parse(previous.Scope) // migrate the previous runtime-only setting
		runID = previous.RunID
		if runID == "" && !sc.SameTargets(eng.Scope()) {
			runID, err = scope.NewRunID()
			if err != nil {
				return err
			}
			if err := scope.SaveOperatorScopeRun(cfg.StateDir, scopeID, sc, runID); err != nil {
				return err
			}
		}
	} else if previous.RunID != "" {
		runID = previous.RunID
	}
	if sc.IsEmpty() && !webUI {
		return fmt.Errorf("engagement has no authorized scope; start with --web to set it before execution, or add scope.targets to the config")
	}
	if previous.Started.IsZero() || runID == "" {
		runID, err = scope.NewRunID()
		if err != nil {
			return err
		}
		if !sc.IsEmpty() {
			if err := scope.SaveOperatorScopeRun(cfg.StateDir, scopeID, sc, runID); err != nil {
				return err
			}
		}
	}
	meta.ConnectionInfo = sc.Describe()

	fmt.Printf("Engagement: %s  (assessment, no flag oracle)\n", eng.Name)
	fmt.Printf("  Authorized scope: %s\n", sc.Describe())
	fmt.Println("  Egress is locked to this scope — out-of-scope hosts are blocked.")
	fmt.Printf("  Models: %s\n\n", strings.Join(specs, ", "))

	stateID := scope.MissionStateID(eng.Name, runID)
	bb, err := blackboard.LoadOrNew(stateID, cfg.StateDir)
	if err != nil {
		return fmt.Errorf("load blackboard: %w", err)
	}
	tl, err := traffic.LoadOrNew(stateID, cfg.StateDir)
	if err != nil {
		return fmt.Errorf("load traffic log: %w", err)
	}

	budget := eng.TimeBudgetSeconds
	if timeLimit > 0 {
		budget = timeLimit
	}
	started := time.Now()
	if !previous.Started.IsZero() && previous.RunID == runID {
		started = previous.Started
	}
	base, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	deadline := time.Time{}
	if budget > 0 {
		deadline = started.Add(time.Duration(budget) * time.Second)
		if previous.RunID == runID && !previous.Deadline.IsZero() && previous.Deadline.Before(deadline) {
			deadline = previous.Deadline
		}
	}
	if err := engagement.SaveRuntime(cfg.StateDir, runtimeID, started, deadline, sc, runID); err != nil {
		return fmt.Errorf("save engagement runtime: %w", err)
	}
	history := &runhistory.Store{StateDir: cfg.StateDir}
	if err := history.Start(stateID, eng.Name, "engagement", sc.Describe(), started); err != nil {
		return fmt.Errorf("record engagement run: %w", err)
	}
	ctx, cancel := context.WithCancel(base)
	defer cancel()

	sw := &swarm.Swarm{
		Meta: meta, Scope: sc, ModelSpecs: specs, Settings: cfg, BB: bb, TL: tl, LLM: llmStore, ModelProfiles: profiles,
		UseConsoleProfile: useConsoleProfile,
	}
	if webUI {
		// Web engagements are operator-led: configure/review Scope and converse
		// while paused, then explicitly resume autonomous execution.
		sw.Pause()
		runner := engrun.New(sw, bb, meta, cancel, deadline)
		runner.SetStarted(started)
		activeRunID := runID
		runner.SetRunFactory(func(next scope.Scope) (engrun.IsolatedRun, error) {
			freshID, err := scope.NewRunID()
			if err != nil {
				return engrun.IsolatedRun{}, err
			}
			freshStateID := scope.MissionStateID(eng.Name, freshID)
			freshBB, err := blackboard.LoadOrNew(freshStateID, cfg.StateDir)
			if err != nil {
				return engrun.IsolatedRun{}, err
			}
			freshTL, err := traffic.LoadOrNew(freshStateID, cfg.StateDir)
			if err != nil {
				return engrun.IsolatedRun{}, err
			}
			freshStarted := time.Now()
			freshDeadline := time.Time{}
			if budget > 0 {
				freshDeadline = freshStarted.Add(time.Duration(budget) * time.Second)
			}
			oldSw, _, _, oldStarted := runner.Current()
			oldScope := oldSw.CurrentScope()
			oldDeadline := runner.Deadline()
			if err := scope.SaveOperatorScopeRun(cfg.StateDir, scopeID, next, freshID); err != nil {
				return engrun.IsolatedRun{}, err
			}
			if err := engagement.SaveRuntime(cfg.StateDir, runtimeID, freshStarted, freshDeadline, next, freshID); err != nil {
				_ = scope.SaveOperatorScopeRun(cfg.StateDir, scopeID, oldScope, activeRunID)
				_ = engagement.SaveRuntime(cfg.StateDir, runtimeID, oldStarted, oldDeadline, oldScope, activeRunID)
				return engrun.IsolatedRun{}, err
			}
			activeRunID = freshID
			freshMeta := eng.ToMeta()
			freshMeta.ConnectionInfo = next.Describe()
			freshSw := &swarm.Swarm{
				Meta: freshMeta, Scope: next, ModelSpecs: specs, Settings: cfg, BB: freshBB, TL: freshTL,
				LLM: llmStore, ModelProfiles: profiles, UseConsoleProfile: useConsoleProfile,
			}
			return engrun.IsolatedRun{Swarm: freshSw, Blackboard: freshBB, Meta: freshMeta,
				Deadline: freshDeadline, Started: freshStarted}, nil
		})
		runner.SetOnRunStopped(func(oldSw *swarm.Swarm, oldBB *blackboard.Blackboard, oldMeta mission.Meta, oldStarted time.Time) {
			archiveDir := filepath.Join(cfg.StateDir, "reports", eng.Name, "runs", oldBB.MissionID)
			oldPaths, err := report.RenderReports(oldBB, archiveDir, report.RunMeta{
				Mode: "assessment", Goal: eng.Goal, Scope: oldSw.CurrentScope().Describe(), Models: specs,
				Started: oldStarted.Format("2006-01-02 15:04:05"), Ended: time.Now().Format("2006-01-02 15:04:05"),
				DurationSeconds: int(time.Since(oldStarted).Seconds()),
			})
			if err != nil {
				fmt.Fprintln(os.Stderr, "archive prior scope report:", err)
			} else if err := report.ArchiveRun(filepath.Join(archiveDir, "archive.tar.gz"), map[string]string{
				"blackboard.json": oldBB.Path, "traffic.jsonl": oldSw.TL.Path,
				"report.md": oldPaths["md"], "report.json": oldPaths["json"], "report.html": oldPaths["html"],
			}); err != nil {
				fmt.Fprintln(os.Stderr, "archive prior scope bundle:", err)
			}
			if err := history.Finish(oldBB.MissionID, "scope_changed", time.Now()); err != nil {
				fmt.Fprintln(os.Stderr, "record prior scope completion:", err)
			}
		})
		runner.SetOnRunStarted(func(newSw *swarm.Swarm, newBB *blackboard.Blackboard, newMeta mission.Meta, newStarted time.Time) {
			if err := history.Start(newBB.MissionID, newMeta.Name, "engagement", newSw.CurrentScope().Describe(), newStarted); err != nil {
				fmt.Fprintln(os.Stderr, "record new scope run:", err)
			}
		})
		done := runner.Start(ctx)
		fmt.Printf("Web console → http://%s:%d (findings: confirmed/suspected/failed)\n\n", webHost, webPort)
		go func() {
			if err := web.ServeWithServices(ctx, runner, llmStore, web.Services{Recorder: llmrec.Shared(cfg.StateDir, cfg.LLMRecord), Guard: guard.Shared(cfg.StateDir, cfg.ToolRulesFile), History: history}, webHost, webPort); err != nil {
				fmt.Fprintln(os.Stderr, "web console error:", err)
			}
		}()
		<-done
		sw, bb, meta, started = runner.Current()
	} else {
		runCtx := ctx
		runCancel := func() {}
		if !deadline.IsZero() {
			runCtx, runCancel = context.WithDeadline(ctx, deadline)
		}
		sw.Run(runCtx)
		runCancel()
	}
	outcome := "completed"
	if base.Err() != nil {
		outcome = "interrupted"
	}
	if err := history.Finish(bb.MissionID, outcome, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "record engagement completion:", err)
	}
	cancel()
	if base.Err() == nil {
		if err := engagement.ClearRuntime(cfg.StateDir, runtimeID); err != nil {
			return fmt.Errorf("clear engagement runtime: %w", err)
		}
		if !sw.CurrentScope().IsEmpty() {
			if err := scope.SaveOperatorScopeRun(cfg.StateDir, scopeID, sw.CurrentScope(), ""); err != nil {
				return fmt.Errorf("prepare next isolated run: %w", err)
			}
		}
	}

	c := bb.Counts()
	fmt.Println("\nAssessment complete")
	fmt.Printf("  Findings: %d (verified %d, candidate %d, inconclusive %d, refuted %d)\n",
		c["findings"], c["findings_verified"], c["findings_candidate"], c["findings_inconclusive"], c["findings_refuted"])
	fmt.Printf("  Facts: %d   Sub-goals: %d   Tokens: %d\n", c["facts"], c["subgoals_total"], sw.TotalTokens())

	rm := report.RunMeta{
		Mode: "assessment", Goal: eng.Goal, Scope: sw.CurrentScope().Describe(), Models: specs,
		Started: started.Format("2006-01-02 15:04:05"), Ended: time.Now().Format("2006-01-02 15:04:05"),
		DurationSeconds: int(time.Since(started).Seconds()),
	}
	reportDir := filepath.Join(cfg.StateDir, "reports", eng.Name)
	paths, err := report.RenderReports(bb, reportDir, rm)
	if err != nil {
		return fmt.Errorf("render report: %w", err)
	}
	runReportDir := filepath.Join(reportDir, "runs", bb.MissionID)
	runPaths, err := report.RenderReports(bb, runReportDir, rm)
	if err != nil {
		return fmt.Errorf("archive isolated report: %w", err)
	}
	if err := report.ArchiveRun(filepath.Join(runReportDir, "archive.tar.gz"), map[string]string{
		"blackboard.json": bb.Path, "traffic.jsonl": sw.TL.Path,
		"report.md": runPaths["md"], "report.json": runPaths["json"], "report.html": runPaths["html"],
	}); err != nil {
		return fmt.Errorf("archive portable run bundle: %w", err)
	}
	fmt.Printf("  Blackboard: %s\n", bb.Path)
	fmt.Printf("  Report: %s\n          %s\n          %s\n", paths["md"], paths["json"], paths["html"])
	return nil
}
