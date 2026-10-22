package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"homelab-notes/internal/core"
)

type ctxKey int

const sessionKey ctxKey = 0

const (
	cookieSecure   = "__Host-notes"
	cookieInsecure = "notes"
	spaCSP         = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' blob:; " +
		"font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"
)

func sessionFrom(r *http.Request) *core.Session { return r.Context().Value(sessionKey).(*core.Session) }

// sessionToken reads only the cookie name valid for this request: over HTTPS the
// __Host- prefixed one, so a sibling subdomain can't inject a plain "notes" cookie.
func (s *Server) sessionToken(r *http.Request) string {
	name := cookieInsecure
	if s.isSecure(r) {
		name = cookieSecure
	}
	if c, err := r.Cookie(name); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	secure := s.isSecure(r)
	name := cookieInsecure
	if secure {
		name = cookieSecure
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: token, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
}

func (s *Server) clearSessionCookies(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieInsecure, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	if s.isSecure(r) {
		http.SetCookie(w, &http.Cookie{Name: cookieSecure, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	}
}

func (s *Server) requireUser(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := s.sessionToken(r)
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "please log in")
			return
		}
		sess, err := s.store.SessionUser(r.Context(), tok)
		if err != nil {
			if errors.Is(err, core.ErrNotFound) {
				s.clearSessionCookies(w, r)
				writeErr(w, http.StatusUnauthorized, "unauthorized", "please log in")
				return
			}
			s.serverError(w, r, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	})
}

func isMutating(m string) bool {
	return m == http.MethodPost || m == http.MethodPut || m == http.MethodPatch || m == http.MethodDelete
}

// csrf: SameSite=Lax cookies + custom header + same-origin Origin/Sec-Fetch-Site checks.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isMutating(r.Method) {
			if r.Header.Get("X-Requested-With") != "notes" {
				writeErr(w, http.StatusForbidden, "csrf", "missing X-Requested-With header")
				return
			}
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				writeErr(w, http.StatusForbidden, "csrf", "cross-site request refused")
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || !strings.EqualFold(u.Host, s.externalHost(r)) {
					writeErr(w, http.StatusForbidden, "csrf", "cross-origin request refused")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// deadlines bounds request bodies for everything except tus PATCH/POST and downloads.
func (s *Server) deadlines(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		long := strings.HasPrefix(r.URL.Path, "/api/uploads/") ||
			(r.Method == http.MethodGet && (strings.HasPrefix(r.URL.Path, "/api/attachments/") || r.URL.Path == "/api/export"))
		if !long {
			rc := http.NewResponseController(w)
			rc.SetReadDeadline(time.Now().Add(60 * time.Second))
			rc.SetWriteDeadline(time.Now().Add(120 * time.Second))
		}
		next.ServeHTTP(w, r)
	})
}

// transferIdle is how long an upload/download may stall before the connection is dropped.
const transferIdle = 2 * time.Minute

// slidingReads extends the read deadline on every read of the request body, so
// long uploads are fine but a stalled one is cut off.
func slidingReads(w http.ResponseWriter, r *http.Request) io.Reader {
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(transferIdle))
	return readerFunc(func(p []byte) (int, error) {
		rc.SetReadDeadline(time.Now().Add(transferIdle))
		return r.Body.Read(p)
	})
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// slidingWrites extends the write deadline on every write.
func slidingWrites(w http.ResponseWriter) http.ResponseWriter {
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Now().Add(transferIdle))
	return &deadlineWriter{ResponseWriter: w, rc: rc}
}

type deadlineWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	d.rc.SetWriteDeadline(time.Now().Add(transferIdle))
	return d.ResponseWriter.Write(p)
}

func (d *deadlineWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		h.Set("Content-Security-Policy", spaCSP)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		if s.isSecure(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" {
			return
		}
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"bytes", sw.bytes, "ms", time.Since(start).Milliseconds(), "ip", s.clientIP(r))
	})
}

// ---- JSON helpers ----

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Extra   any    `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}

func readJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
		} else {
			writeErr(w, http.StatusBadRequest, "bad_json", "invalid JSON body")
		}
		return false
	}
	return true
}

// fail maps core errors to HTTP responses.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var inv *core.InvalidError
	var conf *core.ConflictError
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "not found")
	case errors.As(err, &inv):
		writeErr(w, http.StatusBadRequest, "invalid", inv.Msg)
	case errors.As(err, &conf):
		writeJSON(w, http.StatusConflict, map[string]apiError{"error": {Code: "conflict", Message: conf.Error(),
			Extra: map[string]string{"current_body": conf.CurrentBody, "current_sha256": conf.CurrentSHA256}}})
	case errors.Is(err, core.ErrNoSpace):
		writeErr(w, http.StatusInsufficientStorage, "no_space", "the server is low on disk space; uploads are paused")
	case errors.Is(err, core.ErrRefTaken):
		writeErr(w, http.StatusConflict, "ref_taken", err.Error())
	default:
		s.serverError(w, r, err)
	}
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeErr(w, http.StatusInternalServerError, "internal", "something went wrong")
}
