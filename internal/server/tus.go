package server

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"homelab-notes/internal/core"
)

// Minimal tus 1.0 server: core + creation + creation-with-upload + creation-defer-length + termination.
const tusVersion = "1.0.0"

func tusHeaders(w http.ResponseWriter) {
	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Tus-Extension", "creation,creation-with-upload,creation-defer-length,termination")
}

func checkTus(w http.ResponseWriter, r *http.Request) bool {
	tusHeaders(w)
	if r.Header.Get("Tus-Resumable") != tusVersion {
		w.Header().Set("Tus-Version", tusVersion)
		writeErr(w, http.StatusPreconditionFailed, "tus_version", "unsupported tus version")
		return false
	}
	return true
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func parseMetadata(h string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(h, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(pair), " ")
		if k == "" {
			continue
		}
		dec, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			continue
		}
		out[k] = string(dec)
	}
	return out
}

func (s *Server) tusCreate(w http.ResponseWriter, r *http.Request) {
	if !checkTus(w, r) {
		return
	}
	length := int64(-1)
	if v := r.Header.Get("Upload-Length"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid", "bad Upload-Length")
			return
		}
		length = n
	} else if r.Header.Get("Upload-Defer-Length") != "1" {
		writeErr(w, http.StatusBadRequest, "invalid", "Upload-Length or Upload-Defer-Length required")
		return
	}
	md := parseMetadata(r.Header.Get("Upload-Metadata"))
	a, err := s.store.CreateUpload(r.Context(), uid(r), core.UploadMeta{
		ID: md["id"], Ref: md["ref"], NoteID: md["note_id"], Filename: md["filename"], Length: length,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/uploads/"+a.ID)
	offset := int64(0)
	withBody := r.Header.Get("Content-Type") == "application/offset+octet-stream"
	if withBody || length == 0 {
		var cur int64
		if _, cur, err = s.store.UploadOffset(r.Context(), uid(r), a.ID); err == nil && cur == 0 {
			_, offset, err = s.store.AppendUpload(r.Context(), uid(r), a.ID, 0, -1, slidingReads(w, r))
		} else {
			offset = cur // repeated create of an existing upload: report where it is
		}
		if err != nil && !isClientGone(err) {
			s.fail(w, r, err)
			return
		}
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(offset, 10))
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) tusHead(w http.ResponseWriter, r *http.Request) {
	if !checkTus(w, r) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a, offset, err := s.store.UploadOffset(r.Context(), uid(r), id)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Upload-Offset", strconv.FormatInt(offset, 10))
	w.Header().Set("Upload-Metadata", strings.Join([]string{
		"id " + b64(a.ID), "ref " + b64(a.Ref), "note_id " + b64(a.NoteID), "filename " + b64(a.Filename)}, ","))
	if a.SizeBytes >= 0 {
		w.Header().Set("Upload-Length", strconv.FormatInt(a.SizeBytes, 10))
	} else {
		w.Header().Set("Upload-Defer-Length", "1")
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) tusPatch(w http.ResponseWriter, r *http.Request) {
	if !checkTus(w, r) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if r.Header.Get("Content-Type") != "application/offset+octet-stream" {
		writeErr(w, http.StatusUnsupportedMediaType, "invalid", "Content-Type must be application/offset+octet-stream")
		return
	}
	offset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || offset < 0 {
		writeErr(w, http.StatusBadRequest, "invalid", "bad Upload-Offset")
		return
	}
	newLen := int64(-1)
	if v := r.Header.Get("Upload-Length"); v != "" {
		if newLen, err = strconv.ParseInt(v, 10, 64); err != nil || newLen < 0 {
			writeErr(w, http.StatusBadRequest, "invalid", "bad Upload-Length")
			return
		}
	}
	_, cur, err := s.store.AppendUpload(r.Context(), uid(r), id, offset, newLen, slidingReads(w, r))
	var mm *core.OffsetMismatchError
	switch {
	case errors.As(err, &mm):
		w.Header().Set("Upload-Offset", strconv.FormatInt(mm.Actual, 10))
		writeErr(w, http.StatusConflict, "offset_mismatch", "upload offset mismatch")
		return
	case err != nil && (isClientGone(err) || r.Context().Err() != nil):
		return // client went away; the bytes written so far are kept for resume
	case err != nil:
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(cur, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) tusDelete(w http.ResponseWriter, r *http.Request) {
	if !checkTus(w, r) {
		return
	}
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

// isClientGone reports errors caused by the client disconnecting mid-body.
func isClientGone(err error) bool {
	var inv *core.InvalidError
	if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrNoSpace) || errors.As(err, &inv) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "unexpected EOF") || strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") || strings.Contains(msg, "client disconnected") ||
		strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "context canceled")
}
