package solver

import (
	"encoding/json"
	"strings"
	"testing"
)

// Golden vector produced by real Werkzeug 3.1.8's get_pin_and_cookie_name with
// these exact bits: username=root, modname=xmod, appname=Flask,
// filename=/app/app.py, node=99119950081647 (0x5a2b3c4d5e6f), machine_id=
// fixed-machine-id-1234. Confirmed byte-for-byte against the library.
func TestWerkzeugPINMatchesReferenceVector(t *testing.T) {
	bits := []string{"root", "xmod", "Flask", "/app/app.py", "99119950081647", "fixed-machine-id-1234"}

	pin, cookie := werkzeugPIN(bits, "sha1")
	if pin != "903-556-782" {
		t.Fatalf("sha1 PIN = %q, want 903-556-782", pin)
	}
	if cookie != "__wzd6cffc877656933465f82" {
		t.Fatalf("sha1 cookie = %q, want __wzd6cffc877656933465f82", cookie)
	}

	pin, cookie = werkzeugPIN(bits, "md5")
	if pin != "514-644-682" {
		t.Fatalf("md5 PIN = %q, want 514-644-682", pin)
	}
	if cookie != "__wzd84f8e135a8889df7730d" {
		t.Fatalf("md5 cookie = %q, want __wzd84f8e135a8889df7730d", cookie)
	}
}

func TestWerkzeugPINSkipsEmptyBits(t *testing.T) {
	// Werkzeug skips empty bits entirely, so an empty username must give the same
	// result as omitting it.
	a := werkzeugPINString([]string{"", "flask.app", "Flask", "/app/app.py", "1", "m"})
	b := werkzeugPINString([]string{"flask.app", "Flask", "/app/app.py", "1", "m"})
	if a != b {
		t.Fatalf("empty bit changed the result: %q vs %q", a, b)
	}
}

func werkzeugPINString(bits []string) string {
	p, c := werkzeugPIN(bits, "sha1")
	return p + "|" + c
}

func TestWerkzeugPinToolRequiresPrivateBits(t *testing.T) {
	s := &Solver{}
	out := s.toolWerkzeugPin(`{"username":"root","filename":"/app/app.py"}`)
	if !strings.Contains(out, "required") {
		t.Fatalf("expected a required-field error without node/machine_id, got %q", out)
	}

	out = s.toolWerkzeugPin(`{"username":"root","modname":"xmod","appname":"Flask","filename":"/app/app.py","node":"99119950081647","machine_id":"fixed-machine-id-1234"}`)
	var got struct {
		SHA1 map[string]string `json:"sha1"`
		MD5  map[string]string `json:"md5"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("tool output not JSON: %v (%s)", err, out)
	}
	if got.SHA1["pin"] != "903-556-782" || got.MD5["pin"] != "514-644-682" {
		t.Fatalf("tool PINs = sha1:%q md5:%q, want 903-556-782 / 514-644-682", got.SHA1["pin"], got.MD5["pin"])
	}
}

func transcript(status, reqURL, body string) string {
	return "HTTP " + status + "\nURL: " + reqURL +
		"\n--- response headers ---\nContent-Type: text/html\n--- response body ---\n" + body
}

func TestOutcomeSignatureIgnoresRequestURL(t *testing.T) {
	// Same status and body, DIFFERENT request URL (and different URL length) must
	// collapse to the same signature — the URL echo must never fake a difference.
	a := outcomeSignature(transcript("200 OK", "http://t/x?u=10.0.0.5", "SAME BODY"))
	b := outcomeSignature(transcript("200 OK", "http://t/x?u=0x0a000005000extra", "SAME BODY"))
	if a != b {
		t.Fatalf("request-URL difference leaked into signature:\n %q\n %q", a, b)
	}
}

func TestOutcomeSignatureDistinguishesBodyAndStatus(t *testing.T) {
	base := outcomeSignature(transcript("200 OK", "http://t/x", "BODY A"))
	if base == outcomeSignature(transcript("200 OK", "http://t/x", "BODY B")) {
		t.Fatal("different bodies must differ")
	}
	if base == outcomeSignature(transcript("403 Forbidden", "http://t/x", "BODY A")) {
		t.Fatal("different status must differ")
	}
}

func TestMethodOrDefaultsToGET(t *testing.T) {
	if methodOr("") != "GET" {
		t.Fatalf("empty method should default to GET")
	}
	if methodOr("post") != "POST" {
		t.Fatalf("method should be uppercased")
	}
}
