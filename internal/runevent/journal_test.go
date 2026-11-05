package runevent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Exit immediately after a durable intent, without a result. The parent must
// recover that intent as unknown, never invoke the external action on replay.
func TestProcessRecoveryRetainsIntentWithoutInventingResult(t *testing.T) {
	if path := os.Getenv("CHRONOS_EVENT_TEST_PATH"); path != "" {
		j, err := Open(path, "run")
		if err != nil {
			os.Exit(2)
		}
		if _, err = j.Append("call:intent", "action.intent", map[string]string{"tool": "http_request"}); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "events.jsonl")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessRecoveryRetainsIntentWithoutInventingResult$")
	cmd.Env = append(os.Environ(), "CHRONOS_EVENT_TEST_PATH="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %s %v", out, err)
	}
	j, err := Open(path, "run")
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Events()) != 1 || j.Events()[0].Kind != "action.intent" {
		t.Fatal(j.Events())
	}
	if _, err = j.Append("call:intent", "action.intent", map[string]string{"tool": "http_request"}); err != nil {
		t.Fatal(err)
	}
	if len(j.Events()) != 1 {
		t.Fatal("duplicate commit")
	}
	if _, err = j.Append("call:intent", "action.intent", map[string]string{"tool": "different"}); err == nil {
		t.Fatal("conflicting id accepted")
	}
	if _, err := Open(path, "other-run"); err == nil {
		t.Fatal("cross-run recovery accepted")
	}
}
