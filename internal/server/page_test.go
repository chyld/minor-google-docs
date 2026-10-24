package server

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"homelab-notes/internal/core"
)

func TestNotePageRoute(t *testing.T) {
	h := newHarness(t)
	a := h.login("alice")
	b := h.login("bob")
	note := decode[core.NoteMeta](t, h.do(a, "POST", "/api/notes", `{"body":"Trip plan\nday one"}`, nil))
	path := "/api/notes/" + note.ID + "/html"

	est := decode[map[string]int64](t, h.do(a, "GET", path+"?estimate=1", "", nil))
	res := h.do(a, "GET", path+"?theme=dark", "", nil)
	if res.StatusCode != 200 {
		t.Fatalf("page: %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if int64(len(body)) != est["bytes"] || res.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("estimate %d, length header %s, body %d", est["bytes"], res.Header.Get("Content-Length"), len(body))
	}
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(res.Header.Get("Content-Disposition"), `attachment; filename="Trip plan.html"`) ||
		!strings.Contains(res.Header.Get("Content-Security-Policy"), "default-src 'none'") ||
		res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers: %v", res.Header)
	}
	if !strings.Contains(string(body), `data-theme="dark"`) || !strings.Contains(string(body), "day one") {
		t.Fatal("page body")
	}
	// someone else's note, or a request started by another site
	if res := h.do(b, "GET", path, "", nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("bob: %d", res.StatusCode)
	}
	if res := h.do(b, "GET", path+"?estimate=1", "", nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("bob estimate: %d", res.StatusCode)
	}
	for _, p := range []string{path, "/api/export"} {
		if res := h.do(a, "GET", p, "", map[string]string{"Sec-Fetch-Site": "cross-site"}); res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s cross-site: %d", p, res.StatusCode)
		}
	}
	if res := h.do(a, "GET", path, "", map[string]string{"Sec-Fetch-Site": "same-origin"}); res.StatusCode != 200 {
		t.Fatalf("same-origin: %d", res.StatusCode)
	}
}
