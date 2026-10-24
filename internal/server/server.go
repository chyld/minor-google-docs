// Package server is the HTTP layer: JSON API, tus uploads, attachment serving and the SPA.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"homelab-notes/internal/auth"
	"homelab-notes/internal/config"
	"homelab-notes/internal/core"
)

type Server struct {
	cfg       *config.Config
	store     *core.Store
	log       *slog.Logger
	limiter   *auth.Limiter
	dummyHash string
	doctor    doctorJobs
}

func New(cfg *config.Config, store *core.Store, log *slog.Logger) (*Server, error) {
	dummy, err := auth.HashPassword("dummy password for timing equalization")
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, store: store, log: log, limiter: auth.NewLimiter(), dummyHash: dummy}, nil
}

func (s *Server) Limiter() *auth.Limiter { return s.limiter }

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireUser(h)) }

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.store.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("unhealthy\n"))
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/setup", s.handleSetup)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	api("POST /api/logout", s.handleLogout)
	api("POST /api/logout-others", s.handleLogoutOthers)
	api("GET /api/me", s.handleMe)
	api("PUT /api/me/password", s.handlePassword)
	api("GET /api/me/storage", s.handleStorage)

	api("GET /api/notes", s.handleListNotes)
	api("POST /api/notes", s.handleCreateNote)
	api("GET /api/notes/{id}", s.handleGetNote)
	api("PUT /api/notes/{id}/body", s.handleSaveBody)
	api("PATCH /api/notes/{id}", s.handlePatchNote)
	api("POST /api/notes/{id}/duplicate", s.handleDuplicate)
	api("DELETE /api/notes/{id}", s.handlePurgeNote)
	api("POST /api/trash/empty", s.handleEmptyTrash)

	api("GET /api/tags", s.handleListTags)
	api("POST /api/tags", s.handleCreateTag)
	api("PATCH /api/tags/{id}", s.handlePatchTag)
	api("DELETE /api/tags/{id}", s.handleDeleteTag)

	api("POST /api/uploads/", s.tusCreate)
	api("HEAD /api/uploads/{id}", s.tusHead)
	api("PATCH /api/uploads/{id}", s.tusPatch)
	api("DELETE /api/uploads/{id}", s.tusDelete)

	api("GET /api/attachments/{id}", s.handleServeAttachment)
	api("GET /api/attachments/{id}/meta", s.handleAttachmentMeta)
	api("GET /api/refs/{ref}", s.handleRef)
	api("PATCH /api/attachments/{id}", s.handleRenameAttachment)
	api("DELETE /api/attachments/{id}", s.handleDeleteAttachment)

	api("GET /api/export", s.handleExport)
	api("GET /api/notes/{id}/html", s.handleNotePage)
	api("GET /api/me/doctor", s.handleDoctorStatus)
	api("POST /api/me/doctor", s.handleDoctorStart)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	mux.Handle("/", spaHandler())

	return s.logRequests(s.deadlines(s.securityHeaders(s.csrf(mux))))
}

// ListenAndServe runs the HTTP server until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		// No global Read/WriteTimeout: uploads and downloads can take hours.
		// Per-request deadlines are set in the deadlines middleware.
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	s.log.Info("listening", "addr", s.cfg.Addr)
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// ---- proxy trust ----

func (s *Server) trusted(a netip.Addr) bool {
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

func remoteAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, _ := netip.ParseAddr(host)
	return a.Unmap()
}

// clientIP honors X-Forwarded-For only when the peer is a trusted proxy, and
// takes the right-most address that isn't itself a trusted proxy.
func (s *Server) clientIP(r *http.Request) string {
	peer := remoteAddr(r)
	if !s.trusted(peer) {
		return peer.String()
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		if !s.trusted(a) {
			return a.Unmap().String()
		}
	}
	return peer.String()
}

func (s *Server) isSecure(r *http.Request) bool {
	switch s.cfg.SecureCookies {
	case config.SecureOn:
		return true
	case config.SecureOff:
		return false
	}
	if r.TLS != nil {
		return true
	}
	return s.trusted(remoteAddr(r)) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) externalHost(r *http.Request) string {
	if s.trusted(remoteAddr(r)) {
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			return strings.TrimSpace(strings.Split(h, ",")[0])
		}
	}
	return r.Host
}
