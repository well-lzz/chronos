// Package engagement defines a real assessment mission by an authorized scope.
//
// The operator writes one JSON config describing the goal, the authorized scope
// (hosts / IPs / CIDRs — the HARD egress boundary) and the rules of engagement.
// The scope is not optional: an assessment with an empty or unparseable scope
// fails to load. The config is read-only input; it carries no instructions to
// the model beyond the goal/RoE text.
package engagement

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/scope"
)

// Engagement is one assessment mission: goal + authorized scope + RoE.
type Engagement struct {
	Name              string   `json:"name"`
	Goal              string   `json:"goal"`
	Mode              string   `json:"mode"`
	Category          string   `json:"category"`
	TimeBudgetSeconds int      `json:"time_budget_seconds"`
	ScopeTargets      []string `json:"-"`
	RulesOfEngagement string   `json:"-"`
}

// scopeField allows scope to be either a list of targets or an object
// {targets, rules_of_engagement}.
type rawConfig struct {
	Name              string          `json:"name"`
	Goal              string          `json:"goal"`
	Mode              string          `json:"mode"`
	Category          string          `json:"category"`
	TimeBudgetSeconds int             `json:"time_budget_seconds"`
	RulesOfEngagement string          `json:"rules_of_engagement"`
	Scope             json.RawMessage `json:"scope"`
}

// Load reads a JSON engagement config from path and validates the scope invariant.
func Load(path string) (*Engagement, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("engagement config not found: %s", path)
	}
	var rc rawConfig
	if err := json.Unmarshal(data, &rc); err != nil {
		return nil, fmt.Errorf("engagement config is not valid JSON: %w", err)
	}
	e := &Engagement{
		Name: rc.Name, Goal: rc.Goal, Mode: rc.Mode, Category: rc.Category,
		TimeBudgetSeconds: rc.TimeBudgetSeconds, RulesOfEngagement: rc.RulesOfEngagement,
	}
	if e.Name == "" {
		e.Name = "engagement"
	}
	if e.Mode == "" {
		e.Mode = "assessment"
	}
	if e.Category == "" {
		e.Category = "assessment"
	}
	// scope: list OR {targets, rules_of_engagement}
	if len(rc.Scope) > 0 {
		var asList []string
		if json.Unmarshal(rc.Scope, &asList) == nil {
			e.ScopeTargets = asList
		} else {
			var asObj struct {
				Targets           []string `json:"targets"`
				RulesOfEngagement string   `json:"rules_of_engagement"`
			}
			if json.Unmarshal(rc.Scope, &asObj) == nil {
				e.ScopeTargets = asObj.Targets
				if e.RulesOfEngagement == "" {
					e.RulesOfEngagement = asObj.RulesOfEngagement
				}
			}
		}
	}
	e.ScopeTargets = trimNonEmpty(e.ScopeTargets)
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return e, nil
}

// Validate enforces the authorization boundary: an assessment MUST declare a
// non-empty, parseable scope.
func (e *Engagement) Validate() error {
	if e.Mode == "assessment" {
		if len(e.ScopeTargets) == 0 {
			return fmt.Errorf("engagement %q is an assessment but declares no scope; "+
				"an assessment MUST list explicit authorized targets (scope.targets: "+
				"hosts / IPs / CIDRs). Refusing to run unscoped", e.Name)
		}
		if e.Scope().IsEmpty() {
			return fmt.Errorf("engagement %q scope %v parsed to nothing usable; "+
				"list valid hosts, IPs, or CIDRs", e.Name, e.ScopeTargets)
		}
	}
	return nil
}

func (e *Engagement) Scope() scope.Scope { return scope.Parse(e.ScopeTargets) }

// ToMeta projects the engagement onto the mission descriptor the swarm/solver
// consume. ConnectionInfo carries the scope; Description carries goal + RoE.
func (e *Engagement) ToMeta() mission.Meta {
	var sb strings.Builder
	if e.Goal != "" {
		sb.WriteString("## Assessment goal\n")
		sb.WriteString(strings.TrimSpace(e.Goal) + "\n\n")
	}
	if e.RulesOfEngagement != "" {
		sb.WriteString("## Rules of engagement\n")
		sb.WriteString(strings.TrimSpace(e.RulesOfEngagement) + "\n")
	}
	return mission.Meta{
		Name: e.Name, Category: e.Category,
		Description:    strings.TrimSpace(sb.String()),
		ConnectionInfo: strings.Join(e.ScopeTargets, ", "),
		FlagCount:      0,
	}
}

func trimNonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
