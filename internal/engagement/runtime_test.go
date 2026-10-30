package engagement

import (
	"testing"
	"time"

	"github.com/well-lzz/chronos/internal/scope"
)

func TestEngagementRuntimeWithoutDeadlineRoundTrip(t *testing.T) {
	dir := t.TempDir()
	started := time.Now().UTC().Truncate(time.Second)
	authorized := scope.Parse([]string{"target.test"})
	if err := SaveRuntime(dir, "assessment", started, time.Time{}, authorized); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRuntime(dir, "assessment")
	if err != nil || !got.Started.Equal(started) || !got.Deadline.IsZero() {
		t.Fatalf("unlimited runtime not recovered: %+v %v", got, err)
	}
}

func TestEngagementRuntimeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	deadline := started.Add(20 * time.Minute)
	authorized := scope.Parse([]string{"target.test", "10.20.0.0/16"})
	if err := SaveRuntime(dir, "assessment", started, deadline, authorized); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRuntime(dir, "assessment")
	if err != nil || !got.Started.Equal(started) || !got.Deadline.Equal(deadline) || !scope.Parse(got.Scope).SubsetOf(authorized) {
		t.Fatalf("runtime not recovered: %+v %v", got, err)
	}
	if err := ClearRuntime(dir, "assessment"); err != nil {
		t.Fatal(err)
	}
	if empty, err := LoadRuntime(dir, "assessment"); err != nil || !empty.Started.IsZero() {
		t.Fatal("completed runtime was not removed")
	}
}
