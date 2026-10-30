package browsermcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAvailableChecksPlaywrightNodeRequirement(t *testing.T) {
	for _, test := range []struct {
		version string
		want    bool
	}{
		{"18.20.4", false},
		{"19.9.0", false},
		{"20.20.2", true},
		{"22.0.0", true},
	} {
		t.Run(test.version, func(t *testing.T) {
			dir := t.TempDir()
			node := filepath.Join(dir, "node")
			if err := os.WriteFile(node, []byte("#!/bin/sh\nprintf 'v"+test.version+"\\n'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			cli := filepath.Join(dir, "cli.js")
			if err := os.WriteFile(cli, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CHRONOS_PLAYWRIGHT_NODE", node)
			t.Setenv("CHRONOS_PLAYWRIGHT_CLI", cli)
			if got := Available(); got != test.want {
				t.Fatalf("Available on Node %s = %v, want %v", test.version, got, test.want)
			}
		})
	}
}
