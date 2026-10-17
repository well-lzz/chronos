package scope

import "testing"

func TestScopeHostAndCIDR(t *testing.T) {
	s := Parse([]string{"staging.acme.test", "10.10.5.0/24", "198.51.100.7", "http://app.test:8443/login"})
	cases := map[string]bool{
		"staging.acme.test": true,
		"10.10.5.44":        true, // inside CIDR
		"10.10.5.0":         true,
		"10.10.6.1":         false, // outside CIDR
		"198.51.100.7":      true,
		"198.51.100.8":      false,
		"app.test":          true, // from URL token
		"evil.test":         false,
		"":                  false,
	}
	for host, want := range cases {
		if got := s.Allows(host); got != want {
			t.Errorf("Allows(%q)=%v want %v", host, got, want)
		}
	}
	if len(s.CIDRStrings()) != 1 || s.CIDRStrings()[0] != "10.10.5.0/24" {
		t.Errorf("CIDRStrings=%v", s.CIDRStrings())
	}
}

func TestConnInfoNoRegression(t *testing.T) {
	s := ParseConnInfo("10.0.3.17:9999, 10.0.3.18:80")
	if !s.Allows("10.0.3.17") || !s.Allows("10.0.3.18") || s.Allows("10.0.3.19") {
		t.Fatal("conn-info host parsing wrong")
	}
	if len(s.CIDRStrings()) != 0 {
		t.Fatal("conn-info should yield no CIDRs")
	}
}

func TestEmptyAllowsNothing(t *testing.T) {
	s := Parse(nil)
	if !s.IsEmpty() || s.Allows("127.0.0.1") {
		t.Fatal("empty scope must allow nothing")
	}
}

func TestBracketedIPv6WithPort(t *testing.T) {
	s := Parse([]string{"[::1]:8443"})
	if !s.Allows("::1") {
		t.Fatal("bracketed IPv6 target with port was not normalized")
	}
}
