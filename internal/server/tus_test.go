package server

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"homelab-notes/internal/core"
)

func tusMeta(kv ...string) string {
	var parts []string
	for i := 0; i < len(kv); i += 2 {
		parts = append(parts, kv[i]+" "+base64.StdEncoding.EncodeToString([]byte(kv[i+1])))
	}
	return strings.Join(parts, ",")
}

// The tus HTTP layer: create (with and without a body), HEAD metadata echo,
// chunked PATCH with offset checks, idempotent repeat create, and DELETE.
func TestTusOverHTTP(t *testing.T) {
	h := newHarness(t)
	a := h.login("alice")
	b := h.login("bob")
	note := decode[core.NoteMeta](t, h.do(a, "POST", "/api/notes", `{}`, nil))
	tus := func(extra map[string]string) map[string]string {
		m := map[string]string{"Tus-Resumable": "1.0.0", "Content-Type": ""}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	// wrong protocol version
	if res := h.do(a, "POST", "/api/uploads/", "", map[string]string{"Tus-Resumable": "0.2"}); res.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("bad version: %d", res.StatusCode)
	}

	// create with the whole body in one request
	id1, ref1 := core.NewID(), core.NewRef()
	meta1 := tusMeta("id", id1, "ref", ref1, "note_id", note.ID, "filename", "hello.txt")
	res := h.do(a, "POST", "/api/uploads/", "hello", tus(map[string]string{
		"Upload-Length": "5", "Upload-Metadata": meta1, "Content-Type": "application/offset+octet-stream"}))
	if res.StatusCode != 201 || res.Header.Get("Upload-Offset") != "5" {
		t.Fatalf("create-with-upload: %d offset=%q", res.StatusCode, res.Header.Get("Upload-Offset"))
	}
	// repeating the same create (lost response) reports where it is instead of failing
	res = h.do(a, "POST", "/api/uploads/", "hello", tus(map[string]string{
		"Upload-Length": "5", "Upload-Metadata": meta1, "Content-Type": "application/offset+octet-stream"}))
	if res.StatusCode != 201 || res.Header.Get("Upload-Offset") != "5" {
		t.Fatalf("repeat create: %d offset=%q", res.StatusCode, res.Header.Get("Upload-Offset"))
	}
	// same id, different ref → ref_taken
	res = h.do(a, "POST", "/api/uploads/", "", tus(map[string]string{
		"Upload-Length": "5", "Upload-Metadata": tusMeta("id", id1, "ref", core.NewRef(), "note_id", note.ID, "filename", "x")}))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("ref_taken: %d", res.StatusCode)
	}

	// chunked upload
	id2 := core.NewID()
	res = h.do(a, "POST", "/api/uploads/", "", tus(map[string]string{
		"Upload-Length": "10", "Upload-Metadata": tusMeta("id", id2, "ref", core.NewRef(), "note_id", note.ID, "filename", "ten.bin")}))
	if res.StatusCode != 201 || res.Header.Get("Upload-Offset") != "0" {
		t.Fatalf("create: %d", res.StatusCode)
	}
	head := h.do(a, "HEAD", "/api/uploads/"+id2, "", tus(nil))
	if head.StatusCode != 200 || head.Header.Get("Upload-Offset") != "0" || head.Header.Get("Upload-Length") != "10" {
		t.Fatalf("head: %d %v", head.StatusCode, head.Header)
	}
	if !strings.Contains(head.Header.Get("Upload-Metadata"), "filename "+base64.StdEncoding.EncodeToString([]byte("ten.bin"))) {
		t.Fatalf("head metadata echo: %q", head.Header.Get("Upload-Metadata"))
	}
	patch := func(cookies []*http.Cookie, off, body string) *http.Response {
		return h.do(cookies, "PATCH", "/api/uploads/"+id2, body, tus(map[string]string{
			"Upload-Offset": off, "Content-Type": "application/offset+octet-stream"}))
	}
	if res := patch(a, "0", "01234"); res.StatusCode != 204 || res.Header.Get("Upload-Offset") != "5" {
		t.Fatalf("patch 1: %d", res.StatusCode)
	}
	// wrong offset → 409 with the real offset
	if res := patch(a, "3", "xx"); res.StatusCode != 409 || res.Header.Get("Upload-Offset") != "5" {
		t.Fatalf("offset mismatch: %d %q", res.StatusCode, res.Header.Get("Upload-Offset"))
	}
	// another user can't see or touch it
	if res := patch(b, "5", "56789"); res.StatusCode != 404 {
		t.Fatalf("foreign patch: %d", res.StatusCode)
	}
	if res := h.do(b, "HEAD", "/api/uploads/"+id2, "", tus(nil)); res.StatusCode != 404 {
		t.Fatalf("foreign head: %d", res.StatusCode)
	}
	if res := patch(a, "5", "56789"); res.StatusCode != 204 || res.Header.Get("Upload-Offset") != "10" {
		t.Fatalf("patch 2: %d", res.StatusCode)
	}
	// an empty PATCH at the full offset is an idempotent "finish"
	if res := patch(a, "10", ""); res.StatusCode != 204 {
		t.Fatalf("empty final patch: %d", res.StatusCode)
	}
	got := h.do(a, "GET", "/api/attachments/"+id2, "", nil)
	if data, err := io.ReadAll(got.Body); err != nil || string(data) != "0123456789" {
		t.Fatalf("download: %q %v", data, err)
	}

	// termination
	if res := h.do(b, "DELETE", "/api/uploads/"+id2, "", tus(nil)); res.StatusCode != 404 {
		t.Fatalf("foreign delete: %d", res.StatusCode)
	}
	if res := h.do(a, "DELETE", "/api/uploads/"+id2, "", tus(nil)); res.StatusCode != 204 {
		t.Fatalf("delete: %d", res.StatusCode)
	}
	if res := h.do(a, "HEAD", "/api/uploads/"+id2, "", tus(nil)); res.StatusCode != 404 {
		t.Fatalf("head after delete: %d", res.StatusCode)
	}
}
