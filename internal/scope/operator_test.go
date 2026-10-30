package scope

import (
	"os"
	"testing"
)

func TestOperatorScopeSaveReplaceAndReload(t *testing.T) {
	dir := t.TempDir()
	id := "engagement\x00assessment"
	first, err := ParseOperatorTargets([]string{"https://first.example/path", "10.20.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveOperatorScope(dir, id, first); err != nil {
		t.Fatal(err)
	}
	second, err := ParseOperatorTargets([]string{"second.example", "192.0.2.7"})
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveOperatorScope(dir, id, second); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := LoadOperatorScope(dir, id)
	if err != nil || !found || !loaded.Allows("second.example") || !loaded.Allows("192.0.2.7") || loaded.Allows("first.example") {
		t.Fatalf("saved replacement not restored: found=%v scope=%s err=%v", found, loaded.Describe(), err)
	}
	if _, found, err := LoadOperatorScope(dir, "other mission"); err != nil || found {
		t.Fatalf("scope leaked to another mission: found=%v err=%v", found, err)
	}
	if err := SaveOperatorScopeRun(dir, id, second, "run-two"); err != nil {
		t.Fatal(err)
	}
	_, runID, found, err := LoadOperatorScopeRun(dir, id)
	if err != nil || !found || runID != "run-two" || MissionStateID("mission", runID) == "mission" {
		t.Fatalf("isolated run identity not restored: id=%q found=%v err=%v", runID, found, err)
	}
	if info, err := os.Stat(operatorScopePath(dir, id)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("scope file permissions: info=%v err=%v", info, err)
	}
}

func TestOperatorScopeRejectsMalformedEntries(t *testing.T) {
	for _, entry := range []string{"*", "https://", "192.0.2.0/99", "host.example:bad", "host with space", "ftp://host.example"} {
		if _, err := ParseOperatorTargets([]string{"good.example", entry}); err == nil {
			t.Fatalf("accepted malformed entry %q", entry)
		}
	}
}

func TestOperatorURLAuthorizesItsHost(t *testing.T) {
	s, err := ParseOperatorTargets([]string{"https://target.example?view=summary"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Allows("target.example") || s.Allows("target.example?view=summary") {
		t.Fatalf("URL host normalization failed: %s", s.Describe())
	}
}
