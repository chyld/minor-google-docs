package server

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"homelab-notes/internal/auth"
	"homelab-notes/internal/core"
)

const jsonLimit = 1 << 20

// ---- auth ----

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.UserCount(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"has_users": n > 0, "trash_days": s.cfg.TrashDays})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, 8<<10, &req) {
		return
	}
	// one canonical form for the limiter, the lookup and the audit log
	name := strings.ToLower(strings.TrimSpace(req.Username))
	ip := s.clientIP(r)
	if ok, wait := s.limiter.Allow(name, ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, "rate_limited",
			fmt.Sprintf("Too many attempts. Try again in %s.", wait.Round(time.Second)))
		return
	}
	if !s.limiter.Acquire() {
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, "busy", "Server busy, try again in a moment.")
		return
	}
	user, hash, err := s.store.UserByName(r.Context(), name)
	ok := false
	switch {
	case err == nil:
		ok = auth.VerifyPassword(req.Password, hash) && !user.Disabled
	case errors.Is(err, core.ErrNotFound):
		auth.VerifyPassword(req.Password, s.dummyHash) // equalize timing
	default:
		s.limiter.Release()
		s.serverError(w, r, err)
		return
	}
	s.limiter.Release()
	var token string
	if ok {
		token, err = s.store.CreateSessionVerified(r.Context(), user.ID, hash, r.UserAgent())
		if errors.Is(err, core.ErrNotFound) {
			ok = false // password changed or account disabled while we were verifying
		} else if err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if !ok {
		s.limiter.Fail(name)
		s.store.Audit("ip:"+ip, "login.fail", truncate(name, 64))
		writeErr(w, http.StatusUnauthorized, "bad_credentials", "Wrong username or password.")
		return
	}
	s.limiter.Success(name)
	s.setSessionCookie(w, r, token, int(core.SessionAbsolute.Seconds()))
	s.store.Audit(user.ID, "login.ok", "ip:"+ip)
	writeJSON(w, http.StatusOK, user)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.store.DeleteSession(r.Context(), sessionFrom(r).TokenHash)
	s.clearSessionCookies(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogoutOthers(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	n, err := s.store.DeleteOtherSessions(r.Context(), sess.User.ID, sess.TokenHash)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"revoked": n})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, sessionFrom(r).User)
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !readJSON(w, r, 8<<10, &req) {
		return
	}
	sess := sessionFrom(r)
	if !s.limiter.Acquire() {
		writeErr(w, http.StatusTooManyRequests, "busy", "Server busy, try again in a moment.")
		return
	}
	hash, err := s.store.PasswordHash(r.Context(), sess.User.ID)
	ok := err == nil && auth.VerifyPassword(req.Current, hash)
	s.limiter.Release()
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid", "Current password is wrong.")
		return
	}
	if err := s.store.SetPassword(r.Context(), sess.User.ID, req.New, sess.User.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	token, err := s.store.CreateSession(r.Context(), sess.User.ID, r.UserAgent())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.setSessionCookie(w, r, token, int(core.SessionAbsolute.Seconds()))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.StorageFor(r.Context(), sessionFrom(r).User.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// ---- notes ----

func uid(r *http.Request) string { return sessionFrom(r).User.ID }

func pathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !core.ValidID(id) {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return "", false
	}
	return id, true
}

func (s *Server) handleListNotes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	p := core.ListParams{
		Q: q.Get("q"), Untagged: q.Get("untagged") == "1", Pinned: q.Get("pinned") == "1",
		Trash: q.Get("trash") == "1", HasAttachments: q.Get("has_attachments") == "1",
		Sort: q.Get("sort"), Limit: limit, Cursor: q.Get("cursor"),
	}
	for _, t := range q["tag"] {
		if core.ValidID(t) {
			p.TagIDs = append(p.TagIDs, t)
		}
	}
	list, err := s.store.ListNotes(r.Context(), uid(r), p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateNote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body   string   `json:"body"`
		TagIDs []string `json:"tag_ids"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, core.MaxBodyBytes*2, &req) {
		return
	}
	m, err := s.store.CreateNote(r.Context(), uid(r), req.Body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(req.TagIDs) > 0 {
		if m, err = s.store.PatchNote(r.Context(), uid(r), m.ID, core.NotePatch{TagIDs: &req.TagIDs}); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) handleGetNote(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	n, err := s.store.GetNote(r.Context(), uid(r), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (s *Server) handleSaveBody(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Body       string `json:"body"`
		BaseSHA256 string `json:"base_sha256"`
	}
	// JSON escaping can roughly double the size of the body.
	if !readJSON(w, r, core.MaxBodyBytes*2+4096, &req) {
		return
	}
	res, err := s.store.SaveBody(r.Context(), uid(r), id, req.Body, req.BaseSHA256)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handlePatchNote(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var p core.NotePatch
	if !readJSON(w, r, jsonLimit, &p) {
		return
	}
	m, err := s.store.PatchNote(r.Context(), uid(r), id, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleDuplicate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	m, err := s.store.DuplicateNote(r.Context(), uid(r), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) handlePurgeNote(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.PurgeNote(r.Context(), uid(r), id, uid(r)); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEmptyTrash(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.EmptyTrash(r.Context(), uid(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"deleted": n})
}

// ---- tags ----

func (s *Server) handleListTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.store.ListTags(r.Context(), uid(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": tags, "palette": core.Palette})
}

func (s *Server) handleCreateTag(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if !readJSON(w, r, jsonLimit, &req) {
		return
	}
	t, err := s.store.CreateTag(r.Context(), uid(r), req.Name, req.Color)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handlePatchTag(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var p core.TagPatch
	if !readJSON(w, r, jsonLimit, &p) {
		return
	}
	t, err := s.store.PatchTag(r.Context(), uid(r), id, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteTag(r.Context(), uid(r), id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- attachments ----

func (s *Server) handleServeAttachment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a, path, err := s.store.AttachmentFile(r.Context(), uid(r), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		s.fail(w, r, core.ErrNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	// revalidate every time so access is re-checked (cheap 304 when unchanged)
	h.Set("Cache-Control", "private, no-cache")
	disp := "attachment"
	if a.Inline && r.URL.Query().Get("download") != "1" {
		disp = "inline"
		h.Set("Content-Type", a.Mime)
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	h.Set("Content-Disposition", contentDisposition(disp, a.Filename))
	http.ServeContent(slidingWrites(w), r, "", st.ModTime(), f)
}

// contentDisposition builds an RFC 6266/5987 header from an untrusted filename.
func contentDisposition(disp, name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == '/' || r == ';' {
			return '_'
		}
		return r
	}, name)
	return mime.FormatMediaType(disp, map[string]string{"filename": ascii}) + "; filename*=UTF-8''" + rfc5987(name)
}

func rfc5987(s string) string {
	const ok = "!#$&+-.^_`|~"
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(ok, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (s *Server) handleAttachmentMeta(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a, _, err := s.store.AttachmentFile(r.Context(), uid(r), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleRef(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	if !core.ValidRef(ref) {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	a, err := s.store.AttachmentByRef(r.Context(), uid(r), ref)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleRenameAttachment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Filename string `json:"filename"`
	}
	if !readJSON(w, r, jsonLimit, &req) {
		return
	}
	a, err := s.store.RenameAttachment(r.Context(), uid(r), id, req.Filename)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleDeleteAttachment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteAttachment(r.Context(), uid(r), id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	name := fmt.Sprintf("notes-%s-%s.zip", sess.User.Username, time.Now().Format("2006-01-02"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", name))
	if err := s.store.Export(r.Context(), sess.User.ID, slidingWrites(w)); err != nil {
		s.log.Error("export failed mid-stream", "err", err)
	}
}
