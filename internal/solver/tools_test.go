package solver

import (
	"encoding/json"
	"strings"
	"testing"
)

// Strict schema validators (DeepSeek) reject "required": null. Every tool's
// parameter schema must serialize `required` as an array, including tools with
// no required fields (e.g. traffic_search).
func TestToolSchemasHaveNoNullRequired(t *testing.T) {
	for _, allowExec := range []bool{false, true} {
		for _, hasFlags := range []bool{false, true} {
			defs := toolDefs(allowExec, hasFlags)
			b, err := json.Marshal(defs)
			if err != nil {
				t.Fatalf("marshal tools: %v", err)
			}
			if strings.Contains(string(b), `"required":null`) {
				t.Fatalf("a tool schema serialized required:null (allowExec=%v hasFlags=%v)", allowExec, hasFlags)
			}
		}
	}
}
