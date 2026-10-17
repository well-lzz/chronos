package solver

import (
	"strings"
	"testing"

	"github.com/well-lzz/chronos/internal/scope"
)

func TestBuildToolArgv(t *testing.T) {
	sc := scope.Parse([]string{"119.29.238.216", "example.test"})
	empty := scope.Parse(nil)

	cases := []struct {
		name       string
		tool       string
		target     string
		args       []string
		sc         scope.Scope
		wantArgv   []string // nil => expect an error
		errSubstr  string
	}{
		{"nmap positional", "nmap", "119.29.238.216", []string{"-sV", "-p", "80,443"}, sc,
			[]string{"nmap", "-sV", "-p", "80,443", "119.29.238.216"}, ""},
		{"httpx flag target", "httpx", "http://119.29.238.216:8088", []string{"-title"}, sc,
			[]string{"httpx", "-u", "http://119.29.238.216:8088", "-title"}, ""},
		{"ffuf url with FUZZ", "ffuf", "http://119.29.238.216:8088/FUZZ", nil, sc,
			[]string{"ffuf", "-u", "http://119.29.238.216:8088/FUZZ"}, ""},
		{"disallowed tool", "sqlmap", "119.29.238.216", nil, sc, nil, "not allowed"},
		{"out of scope", "nmap", "8.8.8.8", nil, sc, nil, "not within the authorized scope"},
		{"empty scope refused", "nmap", "119.29.238.216", nil, empty, nil, "NO TARGET KNOWN"},
		{"deny target-file flag", "nmap", "119.29.238.216", []string{"-iL", "hosts.txt"}, sc, nil, "not allowed"},
		{"deny random-target flag", "nmap", "119.29.238.216", []string{"-iR", "100"}, sc, nil, "not allowed"},
		{"reject url in args", "nmap", "119.29.238.216", []string{"http://8.8.8.8/"}, sc, nil, "target URL"},
		{"reject ip in args", "nmap", "119.29.238.216", []string{"8.8.8.8"}, sc, nil, "target IP"},
		{"reject shell meta", "nmap", "119.29.238.216", []string{"-sV; rm -rf /"}, sc, nil, "disallowed character"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			argv, errMsg := buildToolArgv(c.tool, c.target, c.args, c.sc)
			if c.wantArgv == nil {
				if errMsg == "" {
					t.Fatalf("expected error containing %q, got argv %v", c.errSubstr, argv)
				}
				if !strings.Contains(errMsg, c.errSubstr) {
					t.Fatalf("error %q does not contain %q", errMsg, c.errSubstr)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected error: %s", errMsg)
			}
			if strings.Join(argv, " ") != strings.Join(c.wantArgv, " ") {
				t.Fatalf("argv = %v, want %v", argv, c.wantArgv)
			}
		})
	}
}
