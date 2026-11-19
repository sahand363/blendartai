package web

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/sahand363/blendartai/blendmix/internal/config"
	"github.com/sahand363/blendartai/blendmix/internal/runner"
	"github.com/sahand363/blendartai/blendmix/internal/store"
)

//go:embed gallery.html
var galleryFS embed.FS

type Server struct {
	cfg    config.Config
	runner *runner.Runner
	store  *store.Store
	logger *slog.Logger
}

func NewServer(cfg config.Config, r *runner.Runner, st *store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		cfg:    cfg,
		runner: r,
		store:  st,
		logger: logger,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleGallery)
	mux.HandleFunc("POST /api/run", s.requireAuth(s.handleRun))
	mux.HandleFunc("POST /api/stop", s.requireAuth(s.handleStop))
	mux.HandleFunc("GET /api/status", s.requireAuth(s.handleStatus))
	mux.HandleFunc("GET /api/runs", s.requireAuth(s.handleRuns))
	mux.HandleFunc("GET /api/image", s.requireAuth(s.handleImage))
	mux.HandleFunc("POST /api/pick", s.requireAuth(s.handlePick))
	mux.HandleFunc("POST /api/reject", s.requireAuth(s.handleReject))

	if s.cfg.BasePath == "" {
		return mux
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == s.cfg.BasePath {
			http.Redirect(w, r, s.cfg.BasePath+"/", http.StatusMovedPermanently)
			return
		}
		if strings.HasPrefix(r.URL.Path, s.cfg.BasePath+"/") {
			r2 := r.Clone(r.Context())
			r2.URL.Path = strings.TrimPrefix(r.URL.Path, s.cfg.BasePath)
			mux.ServeHTTP(w, r2)
			return
		}
		// Also support reverse proxies that strip BASE_PATH before forwarding.
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !CheckBearerToken(r.Header.Get("Authorization"), s.cfg.Token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="blendmix"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

// CheckBearerToken validates "Authorization: Bearer <token>" using constant-time comparison.
func CheckBearerToken(headerValue, expectedToken string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(headerValue, prefix) {
		return false
	}
	got := strings.TrimPrefix(headerValue, prefix)
	if len(got) != len(expectedToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expectedToken)) == 1
}

func (s *Server) handleGallery(w http.ResponseWriter, r *http.Request) {
	data, err := galleryFS.ReadFile("gallery.html")
	if err != nil {
		http.Error(w, "gallery unavailable", http.StatusInternalServerError)
		return
	}
	html := strings.ReplaceAll(string(data), "__BASE_PATH__", s.cfg.BasePath)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var overrides runner.RunOverrides
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&overrides); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
			return
		}
	}
	st, err := s.runner.Start(overrides)
	if errors.Is(err, runner.ErrAlreadyRunning) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run already in progress"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.runner.Stop()
	writeJSON(w, http.StatusOK, s.runner.Status())
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.runner.Status())
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListRuns()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	runID := r.URL.Query().Get("run")
	relPath := r.URL.Query().Get("path")
	fullPath, err := s.store.ResolveRunFile(runID, relPath)
	if errors.Is(err, store.ErrPathTraversal) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid path"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if _, err := os.Stat(fullPath); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, fullPath)
}

type imageActionReq struct {
	Run  string `json:"run"`
	Path string `json:"path"`
}

func (s *Server) handlePick(w http.ResponseWriter, r *http.Request) {
	var req imageActionReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	dst, err := s.store.PickImage(req.Run, req.Path)
	if errors.Is(err, store.ErrPathTraversal) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid path"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"picked": dst})
}

func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	var req imageActionReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	err := s.store.RejectImage(req.Run, req.Path)
	if errors.Is(err, store.ErrPathTraversal) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid path"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
