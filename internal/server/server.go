// Package server exposes the orchestrator over a JSON HTTP API.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/theognis1002/bursty/internal/api"
	"github.com/theognis1002/bursty/internal/orchestrator"
)

func New(m *orchestrator.Manager) http.Handler {
	s := &server{m: m}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) { reply(w, http.StatusOK, m.Info()) })

	mux.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) { reply(w, http.StatusOK, m.ListVMs()) })
	mux.HandleFunc("POST /v1/vms", s.createVM)
	mux.HandleFunc("GET /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		vm, err := m.GetVM(r.PathValue("id"))
		respond(w, http.StatusOK, vm, err)
	})
	mux.HandleFunc("DELETE /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		err := m.DeleteVM(r.Context(), r.PathValue("id"))
		respond(w, http.StatusOK, map[string]string{"deleted": r.PathValue("id")}, err)
	})
	mux.HandleFunc("POST /v1/vms/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		vm, err := m.StopVM(r.Context(), r.PathValue("id"))
		respond(w, http.StatusOK, vm, err)
	})
	mux.HandleFunc("POST /v1/vms/{id}/pause", func(w http.ResponseWriter, r *http.Request) {
		vm, err := m.PauseVM(r.Context(), r.PathValue("id"))
		respond(w, http.StatusOK, vm, err)
	})
	mux.HandleFunc("POST /v1/vms/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		vm, err := m.ResumeVM(r.Context(), r.PathValue("id"))
		respond(w, http.StatusOK, vm, err)
	})
	mux.HandleFunc("GET /v1/vms/{id}/logs", s.logs)
	mux.HandleFunc("POST /v1/vms/{id}/snapshots", s.createSnapshot)

	mux.HandleFunc("GET /v1/snapshots", func(w http.ResponseWriter, r *http.Request) { reply(w, http.StatusOK, m.ListSnapshots()) })
	mux.HandleFunc("GET /v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		snap, err := m.GetSnapshot(r.PathValue("id"))
		respond(w, http.StatusOK, snap, err)
	})
	mux.HandleFunc("DELETE /v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		err := m.DeleteSnapshot(r.PathValue("id"))
		respond(w, http.StatusOK, map[string]string{"deleted": r.PathValue("id")}, err)
	})
	mux.HandleFunc("POST /v1/snapshots/{id}/restore", s.restore)
	return logRequests(mux)
}

type server struct{ m *orchestrator.Manager }

func (s *server) createVM(w http.ResponseWriter, r *http.Request) {
	var req api.CreateVMRequest
	if !decode(w, r, &req) {
		return
	}
	vm, err := s.m.CreateVM(r.Context(), req)
	respond(w, http.StatusCreated, vm, err)
}

func (s *server) createSnapshot(w http.ResponseWriter, r *http.Request) {
	var req api.CreateSnapshotRequest
	if !decode(w, r, &req) {
		return
	}
	snap, err := s.m.CreateSnapshot(r.Context(), r.PathValue("id"), req)
	respond(w, http.StatusCreated, snap, err)
}

func (s *server) restore(w http.ResponseWriter, r *http.Request) {
	var req api.RestoreRequest
	if !decode(w, r, &req) {
		return
	}
	vm, err := s.m.RestoreSnapshot(r.Context(), r.PathValue("id"), req)
	respond(w, http.StatusCreated, vm, err)
}

// logs returns the serial console; ?follow=1 streams it until the client goes away.
func (s *server) logs(w http.ResponseWriter, r *http.Request) {
	path, err := s.m.ConsolePath(r.PathValue("id"))
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if n, _ := strconv.ParseInt(r.URL.Query().Get("tail"), 10, 64); n > 0 {
		if st, err := f.Stat(); err == nil && st.Size() > n {
			_, _ = f.Seek(-n, io.SeekEnd)
		}
	}
	follow := r.URL.Query().Get("follow") != ""
	flusher, _ := w.(http.Flusher)
	for {
		if _, err := io.Copy(w, f); err != nil || !follow {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength == 0 {
		return true
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		reply(w, http.StatusBadRequest, api.Error{Error: "bad JSON: " + err.Error()})
		return false
	}
	return true
}

func respond(w http.ResponseWriter, ok int, v any, err error) {
	if err == nil {
		reply(w, ok, v)
		return
	}
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, orchestrator.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, orchestrator.ErrConflict):
		code = http.StatusConflict
	case errors.Is(err, orchestrator.ErrInvalid):
		code = http.StatusBadRequest
	}
	reply(w, code, api.Error{Error: err.Error()})
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) { s.code = code; s.ResponseWriter.WriteHeader(code) }

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		h.ServeHTTP(sw, r)
		slog.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.code, "ms", time.Since(t).Milliseconds())
	})
}
