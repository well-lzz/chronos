package solver

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestToolSchemasEncodeRequiredAsArrays(t *testing.T) {
	for _, tool := range toolDefs(true, true) {
		params, ok := tool.Function.Parameters.(map[string]any)
		if !ok {
			t.Fatalf("%s parameters have type %T", tool.Function.Name, tool.Function.Parameters)
		}
		required, ok := params["required"].([]string)
		if !ok {
			t.Fatalf("%s required has type %T", tool.Function.Name, params["required"])
		}
		if required == nil {
			t.Fatalf("%s required must be an empty array, not null", tool.Function.Name)
		}

		encoded, err := json.Marshal(tool)
		if err != nil {
			t.Fatalf("marshal %s: %v", tool.Function.Name, err)
		}
		if bytes.Contains(encoded, []byte(`"required":null`)) {
			t.Fatalf("%s encoded required as null: %s", tool.Function.Name, encoded)
		}
	}
}
