package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"homelab-notes/internal/core"
)

// Check my notes: start, poll to the result, scoped to the signed-in user.
func TestDoctorEndpoints(t *testing.T) {
	h := newHarness(t)
	a := h.login("alice")
	b := h.login("bob")
	note := decode[core.NoteMeta](t, h.do(a, "POST", "/api/notes", `{"body":"alice's note"}`, nil))
	// a problem only alice has: her note's meta.json is gone
	metas, _ := filepath.Glob(filepath.Join(h.store.Dir, "users", "*", "notes", note.ID, "meta.json"))
	if len(metas) != 1 {
		t.Fatal("no meta.json")
	}
	os.Remove(metas[0])

	if res := h.do(a, "POST", "/api/me/doctor", `{"fix":false}`, map[string]string{"X-Requested-With": ""}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("no CSRF header: %d", res.StatusCode)
	}
	if res := h.do(nil, "POST", "/api/me/doctor", `{"fix":false}`, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signed out: %d", res.StatusCode)
	}
	run := func(c []*http.Cookie, fix bool) map[string]any {
		body := `{"fix":false}`
		if fix {
			body = `{"fix":true}`
		}
		if res := h.do(c, "POST", "/api/me/doctor", body, nil); res.StatusCode != http.StatusAccepted {
			t.Fatalf("start: %d", res.StatusCode)
		}
		for range 200 {
			j := decode[map[string]any](t, h.do(c, "GET", "/api/me/doctor", "", nil))
			if j["running"] == false {
				return j
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("check never finished")
		return nil
	}
	problems := func(j map[string]any) []any { return j["report"].(map[string]any)["problems"].([]any) }

	if p := problems(run(b, false)); len(p) != 0 {
		t.Fatalf("bob sees problems: %v", p)
	}
	j := run(a, false)
	if p := problems(j); len(p) != 1 || !strings.Contains(p[0].(string), "alice") && !strings.Contains(p[0].(string), "meta.json") {
		t.Fatalf("alice: %v", j)
	}
	j = run(a, true)
	if fixed := j["report"].(map[string]any)["fixed"].([]any); len(fixed) != 1 {
		t.Fatalf("repair: %v", j)
	}
	if _, err := os.Stat(metas[0]); err != nil {
		t.Fatal("not repaired")
	}
	if p := problems(run(a, false)); len(p) != 0 {
		t.Fatalf("after repair: %v", p)
	}
}

// Deleting the last copy of an image another note waits for answers 409 copy_pending.
func TestDeleteLastCopyIsConflict(t *testing.T) {
	h := newHarness(t)
	a := h.login("alice")
	ctx := context.Background()
	u, _, _ := h.store.UserByName(ctx, "alice")
	src := decode[core.NoteMeta](t, h.do(a, "POST", "/api/notes", `{"body":"src"}`, nil))
	id, ref := core.NewID(), core.NewRef()
	png := "\x89PNG\r\n\x1a\n0123456789"
	if _, err := h.store.CreateUpload(ctx, u.ID, core.UploadMeta{ID: id, Ref: ref, NoteID: src.ID, Filename: "x.png", Length: int64(len(png))}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.AppendUpload(ctx, u.ID, id, 0, -1, strings.NewReader(png)); err != nil {
		t.Fatal(err)
	}
	h.store.Cfg.MinFree = 1 << 62 // copies can't be made
	h.do(a, "POST", "/api/notes", `{"body":"dst\n[image:`+ref+`]"}`, nil)
	res := h.do(a, "DELETE", "/api/attachments/"+id, "", nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("delete: %d", res.StatusCode)
	}
	if e := decode[map[string]map[string]string](t, res); e["error"]["code"] != "copy_pending" {
		t.Fatalf("code: %v", e)
	}
}
