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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/engagement"
	"github.com/well-lzz/chronos/internal/engrun"
	"github.com/well-lzz/chronos/internal/llmconfig"
	"github.com/well-lzz/chronos/internal/platform"
	"github.com/well-lzz/chronos/internal/report"
	"github.com/well-lzz/chronos/internal/scheduler"
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
		engPath   string
		models    modelList
		noExec    bool
		timeLimit int
		webUI     bool
		webHost   string
		webPort   int
		maxCh     int
	)
	flag.StringVar(&engPath, "engagement", "", "run an Assessment engagement from a scope config (JSON)")
	flag.Var(&models, "model", "model spec, repeatable (default deepseek/deepseek-chat)")
	flag.BoolVar(&noExec, "no-exec", false, "disable the run_python tool")
	flag.IntVar(&timeLimit, "time-limit", 0, "per-mission wall-clock seconds (default from config)")
	flag.BoolVar(&webUI, "web", false, "launch the web console alongside the scheduler")
	flag.StringVar(&webHost, "web-host", "127.0.0.1", "web console host")
	flag.IntVar(&webPort, "web-port", 8000, "web console port")
	flag.IntVar(&maxCh, "max-challenges", 0, "concurrent challenge window (default from config, hard cap 3)")
	flag.Parse()

	// Convenience: load KEY=VALUE pairs from a .env in the working directory (and
	// next to the binary) before reading config, so the operator doesn't have to
	// export the API key by hand. Real environment variables always win.
	loadDotEnv(".env")
	if exe, err := os.Executable(); err == nil {
		loadDotEnv(filepath.Join(filepath.Dir(exe), ".env"))
	}

	cfg := config.Load()
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

	// Operator-editable model profile (configured from the console, persisted to
	// the state dir). When set and no --model flag was given, it drives the run;
	// any OpenAI-compatible provider works.
	llmStore := llmconfig.Load(cfg.StateDir)

	specs := []string(models)
	if len(specs) == 0 {
		if p := llmStore.Get(); p.Active() {
			specs = []string{p.Label()}
		} else {
			specs = []string{"deepseek/deepseek-chat"}
		}
	}

	// Engagement mode: one authorized assessment, no platform/scheduler.
	if engPath != "" {
		if err := runEngagement(cfg, engPath, specs, llmStore, webUI, webHost, webPort); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	// Otherwise run the mechanical scheduler against the TSec platform.
	runScheduler(cfg, specs, llmStore, webUI, webHost, webPort)
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

func runScheduler(cfg config.Settings, specs []string, llmStore *llmconfig.Store, webUI bool, webHost string, webPort int) {
	pf, err := platform.New(cfg.PlatformMode, cfg.PlatformBase(), cfg.BenchmarkToken)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	sch := scheduler.New(pf, cfg, specs)
	sch.LLM = llmStore

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("chronos scheduler — platform adapter %q (%s), models %s, window %d\n",
		cfg.PlatformMode, cfg.PlatformBase(), strings.Join(specs, ", "), cfg.MaxConcurrent)
	if webUI {
		fmt.Printf("Web console → http://%s:%d (Ctrl+C to stop)\n", webHost, webPort)
		go func() {
			if err := web.Serve(sch, llmStore, webHost, webPort); err != nil {
				fmt.Fprintln(os.Stderr, "web console error:", err)
			}
		}()
	}
	sch.Run(ctx)
}

func runEngagement(cfg config.Settings, path string, specs []string, llmStore *llmconfig.Store, webUI bool, webHost string, webPort int) error {
	eng, err := engagement.Load(path)
	if err != nil {
		return fmt.Errorf("engagement refused: %w", err)
	}
	meta := eng.ToMeta()
	sc := eng.Scope()

	fmt.Printf("Engagement: %s  (assessment, no flag oracle)\n", eng.Name)
	fmt.Printf("  Authorized scope: %s\n", sc.Describe())
	fmt.Println("  Egress is locked to this scope — out-of-scope hosts are blocked.")
	fmt.Printf("  Models: %s\n\n", strings.Join(specs, ", "))

	bb := blackboard.LoadOrNew(eng.Name, cfg.StateDir)
	tl := traffic.LoadOrNew(eng.Name, cfg.StateDir)

	budget := eng.TimeBudgetSeconds
	if budget <= 0 {
		budget = cfg.ChallengeTimeLimitS
	}
	started := time.Now()
	base, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	deadline := started.Add(time.Duration(budget) * time.Second)
	ctx, cancel := context.WithDeadline(base, deadline)
	defer cancel()

	sw := &swarm.Swarm{
		Meta: meta, Scope: sc, ModelSpecs: specs, Settings: cfg, BB: bb, TL: tl, LLM: llmStore,
	}
	if webUI {
		runner := engrun.New(sw, bb, meta, cancel, deadline)
		fmt.Printf("Web console → http://%s:%d (findings: confirmed/suspected/failed)\n\n", webHost, webPort)
		go func() {
			if err := web.Serve(runner, llmStore, webHost, webPort); err != nil {
				fmt.Fprintln(os.Stderr, "web console error:", err)
			}
		}()
	}
	sw.Run(ctx)

	c := bb.Counts()
	fmt.Println("\nAssessment complete")
	fmt.Printf("  Findings: %d (verified %d, candidate %d, inconclusive %d, refuted %d)\n",
		c["findings"], c["findings_verified"], c["findings_candidate"], c["findings_inconclusive"], c["findings_refuted"])
	fmt.Printf("  Facts: %d   Sub-goals: %d   Tokens: %d\n", c["facts"], c["subgoals_total"], sw.TotalTokens())

	rm := report.RunMeta{
		Mode: "assessment", Goal: eng.Goal, Scope: sc.Describe(), Models: specs,
		Started: started.Format("2006-01-02 15:04:05"), Ended: time.Now().Format("2006-01-02 15:04:05"),
		DurationSeconds: int(time.Since(started).Seconds()),
	}
	reportDir := filepath.Join(cfg.StateDir, "reports", eng.Name)
	paths, err := report.RenderReports(bb, reportDir, rm)
	if err != nil {
		return fmt.Errorf("render report: %w", err)
	}
	fmt.Printf("  Blackboard: %s\n", bb.Path)
	fmt.Printf("  Report: %s\n          %s\n          %s\n", paths["md"], paths["json"], paths["html"])
	return nil
}
