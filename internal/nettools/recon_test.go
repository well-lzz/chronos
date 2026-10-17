package nettools

import "testing"

func TestParsePorts(t *testing.T) {
	cases := []struct {
		in   string
		want []int
		err  bool
	}{
		{"80", []int{80}, false},
		{"80,443,80", []int{80, 443}, false},                 // dedup
		{"8000-8003", []int{8000, 8001, 8002, 8003}, false},  // range
		{"443-440", []int{440, 441, 442, 443}, false},        // reversed range
		{"22, 80 ,443", []int{22, 80, 443}, false},           // spaces
		{"0", []int{}, false},                                // out of range dropped
		{"70000", []int{}, false},                            // out of range dropped
		{"nope", nil, true},                                  // bad token
		{"10-nope", nil, true},                               // bad range
	}
	for _, c := range cases {
		got, errStr := ParsePorts(c.in)
		if c.err {
			if errStr == "" {
				t.Errorf("ParsePorts(%q): expected error, got %v", c.in, got)
			}
			continue
		}
		if errStr != "" {
			t.Errorf("ParsePorts(%q): unexpected error %q", c.in, errStr)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("ParsePorts(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("ParsePorts(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

func TestParsePortsDefault(t *testing.T) {
	got, err := ParsePorts("")
	if err != "" || len(got) == 0 {
		t.Fatalf("empty spec should yield default top ports, got %v err=%q", got, err)
	}
}
