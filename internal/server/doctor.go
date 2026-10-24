package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"homelab-notes/internal/core"
)

// "Check my notes" (SPEC §9.2): a per-user doctor run in the background, so a
// large account can't hit the request deadline. One run per user at a time;
// the last result is kept in memory until the next run.
type doctorJobs struct {
	mu   sync.Mutex
	jobs map[string]*doctorJob
}

type doctorJob struct {
	Running    bool               `json:"running"`
	Fix        bool               `json:"fix"`
	StartedAt  int64              `json:"started_at"`
	FinishedAt int64              `json:"finished_at,omitempty"`
	Report     *core.DoctorReport `json:"report,omitempty"`
	Error      string             `json:"error,omitempty"`
}

func (s *Server) handleDoctorStatus(w http.ResponseWriter, r *http.Request) {
	s.doctor.mu.Lock()
	defer s.doctor.mu.Unlock()
	j := s.doctor.jobs[uid(r)]
	if j == nil {
		writeJSON(w, http.StatusOK, map[string]any{"running": false})
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) handleDoctorStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Fix bool `json:"fix"`
	}
	if !readJSON(w, r, 1<<10, &req) {
		return
	}
	user := uid(r)
	s.doctor.mu.Lock()
	if s.doctor.jobs == nil {
		s.doctor.jobs = map[string]*doctorJob{}
	}
	if j := s.doctor.jobs[user]; j != nil && j.Running {
		s.doctor.mu.Unlock()
		writeErr(w, http.StatusConflict, "busy", "A check is already running.")
		return
	}
	j := &doctorJob{Running: true, Fix: req.Fix, StartedAt: time.Now().UnixMilli()}
	s.doctor.jobs[user] = j
	reply := *j // the goroutine updates j under the lock; answer with a copy
	s.doctor.mu.Unlock()

	go func() {
		// not tied to the request: it finishes even if the page is closed
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		rep, err := s.store.DoctorUser(ctx, user, req.Fix)
		s.doctor.mu.Lock()
		defer s.doctor.mu.Unlock()
		j.Running, j.FinishedAt, j.Report = false, time.Now().UnixMilli(), rep
		if err != nil {
			s.log.Error("check my notes", "err", err)
			j.Error = "The check stopped early because of a server error."
		}
		if req.Fix && rep != nil && len(rep.Fixed) > 0 {
			s.store.Audit(user, "user.doctor.fix", time.Now().UTC().Format(time.RFC3339))
		}
	}()
	writeJSON(w, http.StatusAccepted, reply)
}
