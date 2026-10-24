package server

import (
	"context"
	"io"
	"net/http"
	"os"
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

func TestNotePageEdges(t *testing.T) {
	h := newHarness(t)
	a := h.login("alice")
	ctx := context.Background()
	u, _, _ := h.store.UserByName(ctx, "alice")
	note := decode[core.NoteMeta](t, h.do(a, "POST", "/api/notes", `{"body":"Edges"}`, nil))
	path := "/api/notes/" + note.ID + "/html"

	// HEAD: the headers and size, no body work
	res := h.do(a, "HEAD", path, "", nil)
	if res.StatusCode != 200 || res.Header.Get("Content-Length") == "" {
		t.Fatalf("HEAD: %d %v", res.StatusCode, res.Header)
	}
	// a sibling site is refused too; typed or bookmarked ("none") is fine
	if res := h.do(a, "GET", path, "", map[string]string{"Sec-Fetch-Site": "same-site"}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("same-site: %d", res.StatusCode)
	}
	if res := h.do(a, "GET", path, "", map[string]string{"Sec-Fetch-Site": "none"}); res.StatusCode != 200 {
		t.Fatalf("none: %d", res.StatusCode)
	}
	// an upload still running: 409 with a message
	id, ref := core.NewID(), core.NewRef()
	h.store.CreateUpload(ctx, u.ID, core.UploadMeta{ID: id, Ref: ref, NoteID: note.ID, Filename: "x.png", Length: 10})
	res = h.do(a, "GET", path+"?estimate=1", "", nil)
	if e := decode[map[string]map[string]string](t, res); res.StatusCode != http.StatusConflict || e["error"]["code"] != "upload_pending" {
		t.Fatalf("upload pending: %d %v", res.StatusCode, e)
	}
}

// A file that disappears mid-download resets the connection: the client never gets a
// response it could take for the whole page.
func TestNotePageAbortsMidStream(t *testing.T) {
	h := newHarness(t)
	a := h.login("alice")
	ctx := context.Background()
	u, _, _ := h.store.UserByName(ctx, "alice")
	note := decode[core.NoteMeta](t, h.do(a, "POST", "/api/notes", `{"body":"Big"}`, nil))
	data := strings.Repeat("payload ", 500000)
	id := core.NewID()
	h.store.CreateUpload(ctx, u.ID, core.UploadMeta{ID: id, Ref: core.NewRef(), NoteID: note.ID, Filename: "big.bin", Length: int64(len(data))})
	if _, _, err := h.store.AppendUpload(ctx, u.ID, id, 0, -1, strings.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	core.SetPageBeforeWriteForTest(func(p *core.Page) {
		for _, f := range p.PageFilePaths() {
			os.Truncate(f, 10)
		}
	})
	defer core.SetPageBeforeWriteForTest(nil)
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/notes/"+note.ID+"/html", nil)
	for _, c := range a {
		req.AddCookie(c)
	}
	res, err := http.DefaultClient.Do(req)
	if err == nil {
		_, err = io.ReadAll(res.Body)
		res.Body.Close()
	}
	if err == nil {
		t.Fatal("a broken page download completed normally")
	}
}

// The zip export, too: a file that can't be read mid-export resets the connection.
func TestExportAbortsMidStream(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads anything")
	}
	h := newHarness(t)
	a := h.login("alice")
	ctx := context.Background()
	u, _, _ := h.store.UserByName(ctx, "alice")
	note := decode[core.NoteMeta](t, h.do(a, "POST", "/api/notes", `{"body":"Z"}`, nil))
	id := core.NewID()
	h.store.CreateUpload(ctx, u.ID, core.UploadMeta{ID: id, Ref: core.NewRef(), NoteID: note.ID, Filename: "z.bin", Length: 5})
	att, _, _ := h.store.AppendUpload(ctx, u.ID, id, 0, -1, strings.NewReader("12345"))
	_, path, _ := h.store.AttachmentFile(ctx, u.ID, att.ID)
	os.Chmod(path, 0)
	defer os.Chmod(path, 0o600)
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/export", nil)
	for _, c := range a {
		req.AddCookie(c)
	}
	res, err := http.DefaultClient.Do(req)
	if err == nil {
		_, err = io.ReadAll(res.Body)
		res.Body.Close()
	}
	if err == nil {
		t.Fatal("a broken export completed normally")
	}
}
