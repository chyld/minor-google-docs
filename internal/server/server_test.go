package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"homelab-notes/internal/config"
	"homelab-notes/internal/core"
)

type harness struct {
	t     *testing.T
	srv   *httptest.Server
	store *core.Store
}

func newHarness(t *testing.T, mutate ...func(*config.Config)) *harness {
	t.Helper()
	cfg := &config.Config{DataDir: t.TempDir(), TrashDays: 30, BackupKeep: 3}
	for _, m := range mutate {
		m(cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := core.Open(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	s, err := New(cfg, st, log)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	return &harness{t: t, srv: ts, store: st}
}

func (h *harness) login(name string) []*http.Cookie {
	h.t.Helper()
	if _, err := h.store.CreateUser(context.Background(), name, "correct horse battery"); err != nil {
		h.t.Fatal(err)
	}
	res := h.do(nil, "POST", "/api/login", `{"username":"`+name+`","password":"correct horse battery"}`, nil)
	if res.StatusCode != 200 {
		h.t.Fatalf("login %s: %d", name, res.StatusCode)
	}
	return res.Cookies()
}

func (h *harness) do(cookies []*http.Cookie, method, path, body string, hdr map[string]string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-Requested-With", "notes")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { res.Body.Close() })
	return res
}

func decode[T any](t *testing.T, r *http.Response) T {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Every route must return 404 when user B uses user A's ids.
func TestCrossUserIsolation(t *testing.T) {
	h := newHarness(t)
	a := h.login("alice")
	b := h.login("bob")
	note := decode[core.NoteMeta](t, h.do(a, "POST", "/api/notes", `{"body":"secret"}`, nil))
	tag := decode[core.Tag](t, h.do(a, "POST", "/api/tags", `{"name":"private"}`, nil))
	att := core.NewID()
	ref := core.NewRef()
	if _, err := h.store.CreateUpload(context.Background(), mustUser(t, h, "alice"), core.UploadMeta{ID: att, Ref: ref, NoteID: note.ID, Filename: "f.txt", Length: 0}); err != nil {
		t.Fatal(err)
	}
	h.store.AppendUpload(context.Background(), mustUser(t, h, "alice"), att, 0, -1, strings.NewReader(""))

	cases := []struct{ method, path, body string }{
		{"GET", "/api/notes/" + note.ID, ""},
		{"PUT", "/api/notes/" + note.ID + "/body", `{"body":"x","base_sha256":"` + note.SHA256 + `"}`},
		{"PATCH", "/api/notes/" + note.ID, `{"pinned":true}`},
		{"POST", "/api/notes/" + note.ID + "/duplicate", ""},
		{"DELETE", "/api/notes/" + note.ID, ""},
		{"PATCH", "/api/tags/" + tag.ID, `{"name":"mine"}`},
		{"DELETE", "/api/tags/" + tag.ID, ""},
		{"GET", "/api/attachments/" + att, ""},
		{"GET", "/api/attachments/" + att + "/meta", ""},
		{"PATCH", "/api/attachments/" + att, `{"filename":"x"}`},
		{"DELETE", "/api/attachments/" + att, ""},
		{"GET", "/api/refs/" + ref, ""},
	}
	for _, c := range cases {
		if res := h.do(b, c.method, c.path, c.body, nil); res.StatusCode != 404 {
			t.Errorf("bob %s %s = %d, want 404", c.method, c.path, res.StatusCode)
		}
	}
	// bob can't tag his note with alice's tag
	bn := decode[core.NoteMeta](t, h.do(b, "POST", "/api/notes", `{}`, nil))
	if res := h.do(b, "PATCH", "/api/notes/"+bn.ID, `{"tag_ids":["`+tag.ID+`"]}`, nil); res.StatusCode != 400 {
		t.Errorf("cross-user tag = %d", res.StatusCode)
	}
	// bob's list/search never shows alice's note
	l := decode[core.NoteList](t, h.do(b, "GET", "/api/notes?q=secret", "", nil))
	if len(l.Items) != 0 {
		t.Errorf("search leaked %d notes", len(l.Items))
	}
	// and alice still sees everything
	if res := h.do(a, "GET", "/api/notes/"+note.ID, "", nil); res.StatusCode != 200 {
		t.Errorf("alice lost access: %d", res.StatusCode)
	}
}

func mustUser(t *testing.T, h *harness, name string) string {
	u, _, err := h.store.UserByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func TestCSRFAndAuth(t *testing.T) {
	h := newHarness(t)
	a := h.login("alice")
	if res := h.do(nil, "GET", "/api/notes", "", nil); res.StatusCode != 401 {
		t.Errorf("anonymous = %d", res.StatusCode)
	}
	if res := h.do(a, "POST", "/api/notes", "{}", map[string]string{"X-Requested-With": ""}); res.StatusCode != 403 {
		t.Errorf("missing header = %d", res.StatusCode)
	}
	if res := h.do(a, "POST", "/api/notes", "{}", map[string]string{"Origin": "http://evil.example"}); res.StatusCode != 403 {
		t.Errorf("cross origin = %d", res.StatusCode)
	}
	if res := h.do(a, "POST", "/api/notes", "{}", map[string]string{"Sec-Fetch-Site": "cross-site"}); res.StatusCode != 403 {
		t.Errorf("cross site = %d", res.StatusCode)
	}
	if res := h.do(nil, "POST", "/api/login", `{"username":"alice","password":"x"}`, map[string]string{"Origin": "http://evil.example"}); res.StatusCode != 403 {
		t.Errorf("login csrf = %d", res.StatusCode)
	}
	if res := h.do(a, "POST", "/api/notes", "{}", map[string]string{"Origin": h.srv.URL}); res.StatusCode != 201 {
		t.Errorf("same origin = %d", res.StatusCode)
	}
}

func TestLoginRateLimitAndGenericErrors(t *testing.T) {
	h := newHarness(t)
	h.login("alice")
	bad := func(user string) *http.Response {
		return h.do(nil, "POST", "/api/login", `{"username":"`+user+`","password":"wrong password"}`, nil)
	}
	r1, r2 := bad("alice"), bad("nobody")
	e1, e2 := decode[map[string]apiError](t, r1)["error"], decode[map[string]apiError](t, r2)["error"]
	if r1.StatusCode != 401 || r2.StatusCode != 401 || e1.Message != e2.Message {
		t.Errorf("errors differ: %v vs %v", e1, e2)
	}
	limited := false
	for range 6 {
		if bad("alice").StatusCode == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("no backoff after repeated failures")
	}
}

func TestAttachmentServingHeaders(t *testing.T) {
	h := newHarness(t)
	a := h.login("alice")
	uid := mustUser(t, h, "alice")
	note := decode[core.NoteMeta](t, h.do(a, "POST", "/api/notes", `{}`, nil))
	upload := func(name, content string) string {
		id := core.NewID()
		ctx := context.Background()
		if _, err := h.store.CreateUpload(ctx, uid, core.UploadMeta{ID: id, Ref: core.NewRef(), NoteID: note.ID, Filename: name, Length: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := h.store.AppendUpload(ctx, uid, id, 0, -1, strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		return id
	}
	svg := upload("x.svg", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	res := h.do(a, "GET", "/api/attachments/"+svg, "", nil)
	if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("svg disposition = %q", cd)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("svg content-type = %q", ct)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("csp = %q", csp)
	}
	evil := upload("a\"b\r\nX-Evil: 1.txt", "hi")
	res = h.do(a, "GET", "/api/attachments/"+evil, "", nil)
	if res.Header.Get("X-Evil") != "" || strings.ContainsAny(res.Header.Get("Content-Disposition"), "\r\n") {
		t.Error("header injection via filename")
	}
}

func TestTrustedProxy(t *testing.T) {
	s := &Server{cfg: &config.Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.168.1.5:1234"
	r.Header.Set("X-Forwarded-For", "6.6.6.6")
	if ip := s.clientIP(r); ip != "192.168.1.5" {
		t.Errorf("untrusted peer honored XFF: %s", ip)
	}
	r.RemoteAddr = "10.0.0.2:1234"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9, 10.0.0.3")
	if ip := s.clientIP(r); ip != "203.0.113.9" {
		t.Errorf("trusted chain: %s", ip)
	}
}

func TestLoginLosesRaceWithPasswordReset(t *testing.T) {
	h := newHarness(t)
	h.login("alice")
	ctx := context.Background()
	u, hash, _ := h.store.UserByName(ctx, "alice")
	h.store.SetPassword(ctx, u.ID, "a brand new password", "cli")
	if _, err := h.store.CreateSessionVerified(ctx, u.ID, hash, "test"); err == nil {
		t.Fatal("session issued for a superseded password")
	}
}

func TestSecureCookieNameOnly(t *testing.T) {
	s := &Server{cfg: &config.Config{SecureCookies: config.SecureOn}}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: cookieInsecure, Value: "injected"})
	if tok := s.sessionToken(r); tok != "" {
		t.Fatalf("accepted unprefixed cookie over https: %q", tok)
	}
}

func TestWhitespaceUsernameSharesBackoff(t *testing.T) {
	h := newHarness(t)
	h.login("alice")
	limited := false
	for i := range 8 {
		name := strings.Repeat(" ", i) + "Alice" + strings.Repeat(" ", i)
		res := h.do(nil, "POST", "/api/login", `{"username":"`+name+`","password":"wrong password"}`, nil)
		if res.StatusCode == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("whitespace variants bypassed the per-user backoff")
	}
}
