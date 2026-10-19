package termination

import "testing"

func TestStallProgressNeverPivotsOrTerminates(t *testing.T) {
	p := NewStallPolicy(StallConfig{PivotAfter: 2, PivotGrace: 2, MaxPivots: 2})
	for i := 0; i < 50; i++ {
		if d := p.Observe(true); d.Pivot || d.Terminate {
			t.Fatalf("a progressing executor must never be pivoted or terminated (cycle %d: %+v)", i, d)
		}
	}
}

func TestStallEscalatesThenTerminates(t *testing.T) {
	p := NewStallPolicy(StallConfig{PivotAfter: 3, PivotGrace: 2, MaxPivots: 3})
	var pivots int
	var directives []string
	terminated := false
	// Feed only no-progress cycles; expect 3 pivots then a terminate.
	for i := 0; i < 20; i++ {
		d := p.Observe(false)
		if d.Pivot {
			pivots++
			directives = append(directives, d.Directive)
			if d.Directive == "" {
				t.Fatal("pivot must carry a directive")
			}
		}
		if d.Terminate {
			terminated = true
			break
		}
	}
	if pivots != 3 {
		t.Fatalf("expected 3 escalating pivots, got %d", pivots)
	}
	if directives[0] == directives[1] || directives[1] == directives[2] {
		t.Fatalf("pivot directives must escalate (differ by level): %q / %q / %q", directives[0], directives[1], directives[2])
	}
	if !terminated {
		t.Fatal("expected terminate after the pivot ladder was exhausted")
	}
}

func TestStallFirstPivotHonorsPivotAfter(t *testing.T) {
	p := NewStallPolicy(StallConfig{PivotAfter: 3, PivotGrace: 2, MaxPivots: 3})
	if d := p.Observe(false); d.Pivot {
		t.Fatal("pivot fired after 1 flat cycle, want 3")
	}
	if d := p.Observe(false); d.Pivot {
		t.Fatal("pivot fired after 2 flat cycles, want 3")
	}
	if d := p.Observe(false); !d.Pivot {
		t.Fatal("pivot should fire on the 3rd consecutive flat cycle")
	}
}

func TestStallProgressMidRunResetsStreak(t *testing.T) {
	p := NewStallPolicy(StallConfig{PivotAfter: 3, PivotGrace: 2, MaxPivots: 3})
	p.Observe(false)
	p.Observe(false)   // 2 flat
	p.Observe(true)    // progress resets
	if d := p.Observe(false); d.Pivot {
		t.Fatal("streak did not reset after a progress cycle")
	}
}

// Intermittent progress must not fast-track termination: after burning some
// pivots, a progress cycle resets the ladder, so a later stall pivots again
// instead of terminating.
func TestStallProgressResetsPivotLadder(t *testing.T) {
	p := NewStallPolicy(StallConfig{PivotAfter: 2, PivotGrace: 1, MaxPivots: 2})
	// Drive two pivots (PivotAfter=2 then PivotGrace=1 each).
	p.Observe(false)
	if !p.Observe(false).Pivot { // 2 flat -> pivot 1
		t.Fatal("expected pivot 1")
	}
	if !p.Observe(false).Pivot { // 1 flat (grace) -> pivot 2
		t.Fatal("expected pivot 2")
	}
	// A productive cycle now arrives.
	p.Observe(true)
	// Subsequent stalls must restart the ladder at pivot 1, NOT terminate.
	if d := p.Observe(false); d.Terminate {
		t.Fatal("terminated right after progress — pivot ladder was not reset")
	}
	if d := p.Observe(false); !d.Pivot {
		t.Fatalf("expected a fresh pivot after progress reset, got %+v", d)
	}
}

func TestStallDefaultsApplied(t *testing.T) {
	p := NewStallPolicy(StallConfig{}) // all zero -> defaults 3/2/3
	flat := 0
	for {
		flat++
		if p.Observe(false).Pivot {
			break
		}
		if flat > 10 {
			t.Fatal("default PivotAfter not applied")
		}
	}
	if flat != 3 {
		t.Fatalf("default PivotAfter = %d flat cycles, want 3", flat)
	}
}
