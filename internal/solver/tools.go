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
func toolDefs(allowExec, hasFlags bool) []openai.Tool {
	defs := []struct {
		name, desc string
		params     map[string]any
	}{
		{"http_request", "Full-control HTTP to an in-scope target. Returns status/headers/body and a [traffic tN] id to cite as evidence.",
			obj(map[string]any{
				"url":     strProp("target URL"),
				"method":  strProp("HTTP method (default GET)"),
				"headers": map[string]any{"type": "object", "description": "request headers", "additionalProperties": map[string]any{"type": "string"}},
				"body":    strProp("request body"),
			}, "url")},
		{"tcp_send", "One raw TCP exchange with an in-scope host. Returns the reply and a traffic id.",
			obj(map[string]any{
				"host":        strProp("target host"),
				"port":        map[string]any{"type": "integer"},
				"payload":     strProp("text payload (line protocols)"),
				"payload_hex": strProp("hex payload (binary protocols)"),
			}, "host", "port")},
		{"werkzeug_pin", "Compute the Werkzeug debugger PIN (and __wzd cookie) from bits scraped off the target: username, module name, app name, app file path, the MAC as uuid.getnode()'s DECIMAL, and the machine id (/etc/machine-id or /proc/sys/kernel/random/boot_id). Returns both the sha1 (Werkzeug >=0.15) and md5 (<0.15) PINs — no network, pure computation.",
			obj(map[string]any{
				"username":   strProp("OS user running the app (e.g. root, www-data)"),
				"modname":    strProp("app module name, default flask.app"),
				"appname":    strProp("app object name, default Flask"),
				"filename":   strProp("absolute path of the app's module file (from the traceback), e.g. /app/app.py"),
				"node":       strProp("uuid.getnode() as a DECIMAL string (the MAC of the first NIC converted to base-10)"),
				"machine_id": strProp("contents of /etc/machine-id, or boot_id, concatenated as Werkzeug does"),
			}, "node", "machine_id")},
		{"race", "Fire N identical requests concurrently at one in-scope URL to probe for TOCTOU/race conditions. Reports distinct outcomes and cites one captured exchange per outcome. Scope-enforced like http_request.",
			obj(map[string]any{
				"url":     strProp("target URL (in scope)"),
				"method":  strProp("HTTP method (default GET)"),
				"headers": map[string]any{"type": "object", "description": "request headers", "additionalProperties": map[string]any{"type": "string"}},
				"body":    strProp("request body"),
				"count":   map[string]any{"type": "integer", "description": "number of concurrent requests (default 10, max 50)"},
			}, "url")},
		{"add_fact", "Record a confirmed fact about the target (world state).",
			obj(map[string]any{"statement": strProp("the fact"), "evidence": strProp("optional evidence")}, "statement")},
		{"add_finding", "Record a finding (a product of the search). Record candidates too (verified=false). Cite real traffic in evidence_refs.",
			obj(map[string]any{
				"title":         strProp("short title"),
				"severity":      map[string]any{"type": "string", "enum": []string{"info", "low", "medium", "high", "critical"}},
				"target":        strProp("affected target"),
				"category":      strProp("descriptive category, e.g. web/injection"),
				"evidence":      strProp("what you observed"),
				"reproduction":  strProp("how to reproduce"),
				"verified":      map[string]any{"type": "boolean"},
				"evidence_refs": evidenceRefsSchema,
			}, "title")},
		{"verify_finding", "Record a verification attempt. verdict: reproduced (->verified) | refuted | inconclusive (say why in evidence). Appends a record; original evidence is kept.",
			obj(map[string]any{
				"finding_id":    strProp("finding id, e.g. v3"),
				"verdict":       map[string]any{"type": "string", "enum": []string{"reproduced", "refuted", "inconclusive"}},
				"summary":       strProp("one-line verdict summary"),
				"evidence":      strProp("actual steps/observations, or the blocker reason"),
				"evidence_refs": evidenceRefsSchema,
			}, "finding_id", "verdict")},
		{"add_subgoal", "Add something still worth investigating (your coverage checklist).",
			obj(map[string]any{"description": strProp("the sub-goal")}, "description")},
		{"resolve_subgoal", "Mark a sub-goal resolved with a one-line outcome.",
			obj(map[string]any{"subgoal_id": strProp("sub-goal id, e.g. g2"), "outcome": strProp("outcome")}, "subgoal_id")},
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
		}{"run_python", "Local CPython scratchpad (if available). Its sockets are scope-locked. print() what you need.",
			obj(map[string]any{"code": strProp("python source")}, "code")})
	}
	if hasFlags {
		defs = append(defs, struct {
			name, desc string
			params     map[string]any
		}{"submit_flag", "Submit a captured flag verbatim; recorded as a verified finding.",
			obj(map[string]any{"flag": strProp("the flag")}, "flag")})
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
