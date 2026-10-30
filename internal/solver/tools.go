package solver

import openai "github.com/sashabaranov/go-openai"

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

var evidenceRefsSchema = map[string]any{
	"type":        "array",
	"description": "real captured traffic cited as evidence",
	"items": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ref":  map[string]any{"type": "string", "description": "traffic id, e.g. t5"},
			"role": map[string]any{"type": "string", "enum": []string{"baseline", "proof", "verification", "supporting"}},
			"note": map[string]any{"type": "string"},
		},
		"required": []string{"ref"},
	},
}

func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

// toolDefs returns the function-calling tool set offered to the model.
func toolDefs(allowExec, hasFlags bool, canDelegate ...bool) []openai.Tool {
	defs := []struct {
		name, desc string
		params     map[string]any
	}{
		{"http_request", "Full-control HTTP to an in-scope target. GET/HEAD need no claim; before state-changing methods claim the host:port or endpoint family with claim_surface. Returns status/headers/body and a [traffic tN] id to cite as evidence.",
			obj(map[string]any{
				"url":     strProp("target URL"),
				"method":  strProp("HTTP method (default GET)"),
				"headers": map[string]any{"type": "object", "description": "request headers", "additionalProperties": map[string]any{"type": "string"}},
				"body":    strProp("request body"),
			}, "url")},
		{"tcp_send", "One raw TCP exchange with an in-scope host. Claim host:port first because the protocol may change target state. Returns the reply and a traffic id.",
			obj(map[string]any{
				"host":        strProp("target host"),
				"port":        map[string]any{"type": "integer"},
				"payload":     strProp("text payload (line protocols)"),
				"payload_hex": strProp("hex payload (binary protocols)"),
			}, "host", "port")},
		{"add_fact", "Record a factual claim. Attach captured traffic as evidence_refs when available; only proof or verification traffic can confirm it. from_ids records lineage.",
			obj(map[string]any{"statement": strProp("the factual claim"), "evidence": strProp("optional observation text"), "evidence_refs": evidenceRefsSchema, "from_ids": map[string]any{"type": "array", "items": strProp("source fact, finding or sub-goal ID")}}, "statement")},
		{"add_finding", "Record a finding claim with real traffic and an executable runbook. The evidence graph computes its verdict; independent verification is still assigned. Reproduction must include numbered steps and Expected: criterion.",
			obj(map[string]any{
				"title":         strProp("short root-cause title; do not duplicate an existing finding"),
				"severity":      map[string]any{"type": "string", "enum": []string{"info", "low", "medium", "high", "critical"}},
				"target":        strProp("affected target"),
				"category":      strProp("descriptive category, e.g. web/injection"),
				"evidence":      strProp("what you actually observed"),
				"reproduction":  strProp("numbered executable steps plus Expected: observable success criterion; preserve exact payloads and document dynamic value extraction"),
				"evidence_refs": evidenceRefsSchema,
				"from_ids":      map[string]any{"type": "array", "items": strProp("source blackboard ID")},
			}, "title", "evidence", "reproduction", "evidence_refs")},
		{"verify_finding", "Independently execute the existing reproduction before variants. verdict: reproduced (->verified) | refuted | inconclusive. Reproduced/refuted requires actual observations and new replay traffic cited with role=verification. Appends a record; original evidence is kept.",
			obj(map[string]any{
				"finding_id":    strProp("finding id, e.g. v3"),
				"verdict":       map[string]any{"type": "string", "enum": []string{"reproduced", "refuted", "inconclusive"}},
				"summary":       strProp("one-line verdict summary"),
				"evidence":      strProp("actual replay steps/observations, or the blocker reason; never prose-only speculation"),
				"evidence_refs": evidenceRefsSchema,
			}, "finding_id", "verdict", "summary", "evidence", "evidence_refs")},
		{"challenge_finding", "Create a counterclaim that refutes an existing finding or challenge. Attach captured traffic as evidence_refs; proof/verification traffic can defeat the target, while an ungrounded challenge leaves it suspected.",
			obj(map[string]any{"target_id": strProp("claim ID to challenge, e.g. v3 or c8"), "reason": strProp("concrete contradicting observation"), "evidence_refs": evidenceRefsSchema}, "target_id", "reason")},
		{"relate_findings", "Record a supports or refutes edge between two existing claims. A refuter only wins if independently grounded by proof traffic.",
			obj(map[string]any{"a": strProp("source claim ID"), "b": strProp("target claim ID"), "kind": map[string]any{"type": "string", "enum": []string{"supports", "refutes"}}}, "a", "b", "kind")},
		{"add_subgoal", "Add something still worth investigating (your coverage checklist). Link the records that led to it with from_ids.",
			obj(map[string]any{"description": strProp("the sub-goal"), "from_ids": map[string]any{"type": "array", "items": strProp("source blackboard ID")}}, "description")},
		{"resolve_subgoal", "Mark a sub-goal resolved with a one-line outcome.",
			obj(map[string]any{"subgoal_id": strProp("sub-goal id, e.g. g2"), "outcome": strProp("outcome")}, "subgoal_id")},
		{"claim_surface", "Claim an authorized host:port or HTTP(S) endpoint family before state-changing work. Claims are shared across peer executors and block conflicting writes. Link an open sub-goal when relevant; resolve or release when done.",
			obj(map[string]any{"surface": strProp("HTTP(S) URL, host:port, or mission:* for run_python"), "subgoal_id": strProp("optional open sub-goal id"), "ttl_seconds": map[string]any{"type": "integer", "description": "30–300 seconds; default 120"}}, "surface")},
		{"release_surface", "Release one of your claims when finished so peers can work on that surface.",
			obj(map[string]any{"surface": strProp("the surface previously claimed")}, "surface")},
		{"traffic_search", "Search traffic you ALREADY captured (no re-fetch). body_contains is full-text (>=3 chars). Returns a light index.",
			obj(map[string]any{
				"host": strProp("host filter"), "url_contains": strProp("url substring"),
				"body_contains": strProp("full-text substring (>=3 chars)"), "page": map[string]any{"type": "integer"},
			})},
		{"traffic_get", "Read one captured exchange by id. side: request|response|both. Bounded to 8192-byte slices.",
			obj(map[string]any{
				"traffic_id": strProp("exchange id, e.g. t5"), "side": strProp("request|response|both"),
				"offset": map[string]any{"type": "integer"}, "length": map[string]any{"type": "integer"},
			}, "traffic_id")},
		{"notify", "Report a blocker or status upward (logged).",
			obj(map[string]any{"message": strProp("the message")}, "message")},
	}
	if allowExec {
		defs = append(defs, struct {
			name, desc string
			params     map[string]any
		}{"run_python", "Local CPython scratchpad (if available). Claim mission:* first: code may change target state through any in-scope socket. Its sockets remain scope-locked. print() what you need.",
			obj(map[string]any{"code": strProp("python source")}, "code")})
	}
	if hasFlags {
		defs = append(defs, struct {
			name, desc string
			params     map[string]any
		}{"submit_flag", "Submit a captured flag verbatim; recorded as a verified finding.",
			obj(map[string]any{"flag": strProp("the flag")}, "flag")})
	}
	if len(canDelegate) > 0 && canDelegate[0] {
		defs = append(defs,
			struct {
				name, desc string
				params     map[string]any
			}{
				"delegate", "Delegate a bounded, self-contained investigation to a fresh context when its intermediate tool traffic would greatly exceed the value of its conclusion. Include all needed context in task. Keep quick queries, main-line reasoning, and context-dependent work here. Child shares the blackboard and authorized scope; only concise results return. Multiple independent calls may run concurrently within limits.",
				obj(map[string]any{
					"task":      strProp("complete stand-alone sub-investigation with target, known facts, exclusions and expected result"),
					"max_steps": map[string]any{"type": "integer", "description": "requested tool-step budget; harness applies a stricter cap"},
					"focus":     strProp("optional host/IP/CIDR subset of current authorized scope; never expands scope"),
				}, "task", "max_steps"),
			},
			struct {
				name, desc string
				params     map[string]any
			}{
				"delegate_resume", "Resume one of your direct unfinished child agents in its original isolated context. Use the returned agent_id; provide a small extra step budget and optional new guidance.",
				obj(map[string]any{
					"agent_id":    strProp("direct child handle returned by delegate"),
					"extra_steps": map[string]any{"type": "integer", "description": "additional requested tool steps, subject to the same hard caps"},
					"note":        strProp("optional follow-up guidance for the child"),
				}, "agent_id", "extra_steps"),
			},
		)
	}

	out := make([]openai.Tool, 0, len(defs))
	for _, d := range defs {
		out = append(out, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name: d.name, Description: d.desc, Parameters: d.params,
			},
		})
	}
	return out
}
