package durable

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPrivateSnapshotAndProcessLock(t *testing.T) {
	if dir := os.Getenv("CHRONOS_LOCK_TEST_DIR"); dir != "" {
		lock, err := LockDir(dir)
		if err == nil {
			lock.Close()
			os.Exit(4)
		}
		os.Exit(0)
	}
	dir := t.TempDir()
	lock, err := LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPrivateSnapshotAndProcessLock$")
	cmd.Env = append(os.Environ(), "CHRONOS_LOCK_TEST_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("another process acquired lock: %s %v", out, err)
	}
	lock.Close()
	lock, err = LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	path := filepath.Join(dir, "state.json")
	if err := WriteFile(path, []byte(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte(`{"version":2}`)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	info, _ := os.Stat(path)
	if err != nil || string(data) != `{"version":2}` || info.Mode().Perm() != 0600 {
		t.Fatalf("bad snapshot: %s %v", data, err)
	}
}
