package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"homelab-notes/internal/core"
)

func TestNoteArchiveRoute(t *testing.T) {
	h := newHarness(t)
	a := h.login("alice")
	b := h.login("bob")
	ctx := context.Background()
	u, _, _ := h.store.UserByName(ctx, "alice")
	note := decode[core.NoteMeta](t, h.do(a, "POST", "/api/notes", `{"body":"Trip plan\nday one"}`, nil))
	path := "/api/notes/" + note.ID + "/archive"

	if c := decode[map[string]int](t, h.do(a, "GET", path+"?check=1", "", nil)); c["missing"] != 0 {
		t.Fatalf("check: %v", c)
	}
	res := h.do(a, "GET", path, "", nil)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/gzip" ||
		!strings.Contains(res.Header.Get("Content-Disposition"), `attachment; filename="Trip plan.tar.gz"`) ||
		res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("archive: %d %v", res.StatusCode, res.Header)
	}
	gz, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	names := map[string]bool{}
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		names[hd.Name] = true
	}
	if !names["notes/Trip plan/note.txt"] || !names["manifest.json"] {
		t.Fatalf("entries: %v", names)
	}
	if res := h.do(a, "HEAD", path, "", nil); res.StatusCode != 200 {
		t.Fatalf("HEAD: %d", res.StatusCode)
	}
	// someone else's note, or a request started by another site (a sibling site too)
	for _, p := range []string{path, path + "?check=1"} {
		if res := h.do(b, "GET", p, "", nil); res.StatusCode != http.StatusNotFound {
			t.Fatalf("bob %s: %d", p, res.StatusCode)
		}
	}
	for _, site := range []string{"cross-site", "same-site"} {
		if res := h.do(a, "GET", path, "", map[string]string{"Sec-Fetch-Site": site}); res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: %d", site, res.StatusCode)
		}
	}
	if res := h.do(a, "GET", path, "", map[string]string{"Sec-Fetch-Site": "none"}); res.StatusCode != 200 {
		t.Fatalf("none: %d", res.StatusCode)
	}
	// an upload still running, then a trashed note: 409 with a message
	id := core.NewID()
	h.store.CreateUpload(ctx, u.ID, core.UploadMeta{ID: id, Ref: core.NewRef(), NoteID: note.ID, Filename: "x.png", Length: 10})
	res = h.do(a, "GET", path+"?check=1", "", nil)
	if e := decode[map[string]map[string]string](t, res); res.StatusCode != http.StatusConflict || e["error"]["code"] != "upload_pending" ||
		strings.Contains(e["error"]["message"], "page") {
		t.Fatalf("upload pending: %d %v", res.StatusCode, e)
	}
	h.store.DB.W.Exec(`DELETE FROM attachments WHERE id=?`, id)
	h.do(a, "PATCH", "/api/notes/"+note.ID, `{"trashed":true}`, nil)
	for _, p := range []string{path, path + "?check=1"} {
		res = h.do(a, "GET", p, "", nil)
		if e := decode[map[string]map[string]string](t, res); res.StatusCode != http.StatusConflict || e["error"]["code"] != "note_trashed" {
			t.Fatalf("trashed %s: %d %v", p, res.StatusCode, e)
		}
	}
}

// ?check=1 reports files missing on the server, so the client can say so.
func TestNoteArchiveCheckCountsMissing(t *testing.T) {
	h := newHarness(t)
	a := h.login("alice")
	note := decode[core.NoteMeta](t, h.do(a, "POST", "/api/notes", `{"body":"Pics\n[image:aaaaaaaaaa] gone"}`, nil))
	if c := decode[map[string]int](t, h.do(a, "GET", "/api/notes/"+note.ID+"/archive?check=1", "", nil)); c["missing"] != 1 {
		t.Fatalf("check: %v", c)
	}
}

// A file that changes mid-download resets the connection: the client never gets an
// archive it could take for the whole one.
func TestNoteArchiveAbortsMidStream(t *testing.T) {
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
	// shrinks on every attempt: Go's client retries a request reset before any byte
	// arrives, and each retry snapshots the file anew
	core.SetArchiveBeforeWriteForTest(func(ar *core.NoteArchive) {
		for _, f := range ar.ArchiveFilePaths() {
			if st, err := os.Stat(f); err == nil {
				os.Truncate(f, st.Size()-1)
			}
		}
	})
	defer core.SetArchiveBeforeWriteForTest(nil)
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/notes/"+note.ID+"/archive", nil)
	for _, c := range a {
		req.AddCookie(c)
	}
	res, err := http.DefaultClient.Do(req)
	if err == nil {
		_, err = io.ReadAll(res.Body)
		res.Body.Close()
	}
	if err == nil {
		t.Fatal("a broken archive download completed normally")
	}
}
