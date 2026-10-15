// Command chronos runs one Assessment-mode engagement: a single multi-target
// mission with a shared, persisted blackboard and real-traffic evidence. No
// flag oracle — findings are the product, written to a local report.
//
//	chronos --engagement path.json
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/engagement"
	"github.com/well-lzz/chronos/internal/report"
	"github.com/well-lzz/chronos/internal/swarm"
	"github.com/well-lzz/chronos/internal/traffic"
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
	)
	flag.StringVar(&engPath, "engagement", "", "path to an engagement scope config (JSON)")
	flag.Var(&models, "model", "model spec, repeatable (default deepseek/deepseek-chat)")
	flag.BoolVar(&noExec, "no-exec", false, "disable the run_python tool")
	flag.IntVar(&timeLimit, "time-limit", 0, "wall-clock seconds for the mission (default from config)")
	flag.Parse()

	if engPath == "" {
		fmt.Fprintln(os.Stderr, "usage: chronos --engagement <config.json>")
		os.Exit(2)
	}

	cfg := config.Load()
	if noExec {
		cfg.AllowLocalExec = false
	}
	if timeLimit > 0 {
		cfg.ChallengeTimeLimitS = timeLimit
	}

	specs := []string(models)
	if len(specs) == 0 {
		specs = []string{"deepseek/deepseek-chat"}
	}

	if err := runEngagement(cfg, engPath, specs); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runEngagement(cfg config.Settings, path string, specs []string) error {
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(budget)*time.Second)
	defer cancel()

	sw := &swarm.Swarm{
		Meta: meta, Scope: sc, ModelSpecs: specs, Settings: cfg, BB: bb, TL: tl,
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
