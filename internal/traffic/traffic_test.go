package traffic

import "testing"

func TestRecordSearchGetPersist(t *testing.T) {
	dir := t.TempDir()
	l, err := LoadOrNew("m1", dir)
	if err != nil {
		t.Fatal(err)
	}
	t1, err := l.RecordHTTP("GET", "http://h.test/search?q=x", map[string]string{"Cookie": "a=1"}, "",
		"HTTP 200 OK\n--- response body ---\n<script> reflected", "deepseek")
	if err != nil {
		t.Fatal(err)
	}
	t2, err := l.RecordHTTP("POST", "http://h.test/login", nil, `{"u":"x"}`, "HTTP 401 Unauthorized", "deepseek")
	if err != nil {
		t.Fatal(err)
	}
	if t1 != "t1" || t2 != "t2" {
		t.Fatalf("ids %s %s", t1, t2)
	}
	s := l.Search("", "", "reflected", 0)
	if s["total"].(int) != 1 {
		t.Fatalf("search total=%v", s["total"])
	}
	g := l.Get("t1", "response", 0, 8192)
	if _, ok := g["response"]; !ok {
		t.Fatal("get response missing")
	}
	if !l.Exists("t2") || l.Exists("t9") {
		t.Fatal("exists wrong")
	}
	if l.Search("h.test", "", "", 0)["total"].(int) != 2 {
		t.Fatal("host search wrong")
	}
	// reload from JSONL
	l2, err := LoadOrNew("m1", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !l2.Exists("t1") || !l2.Exists("t2") || l2.seq != 2 {
		t.Fatalf("reload wrong seq=%d", l2.seq)
	}
}

func TestStatusAndMinSearch(t *testing.T) {
	l, err := LoadOrNew("m2", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.RecordHTTP("GET", "http://x/", nil, "", "HTTP 500 err", "x"); err != nil {
		t.Fatal(err)
	}
	if got := l.Search("", "", "ab", 0); got["error"] == nil {
		t.Fatal("min search length not enforced")
	}
}
