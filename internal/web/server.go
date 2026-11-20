// Package web serves the Blendartai panel and its JSON API.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sahand363/blendartai/internal/auth"
	"github.com/sahand363/blendartai/internal/config"
	"github.com/sahand363/blendartai/internal/runner"
	"github.com/sahand363/blendartai/internal/store"
)

//go:embed app.html
var assets embed.FS

const (
	cookieName    = "blendartai_session"
	csrfHeader    = "X-Blendartai"
	maxUploadBody = 400 << 20 // whole multipart request
	maxJSONBody   = 1 << 20
)

type Options struct {
	Version    string
	BasePath   string
	TrustProxy bool // honour X-Forwarded-Proto / X-Forwarded-For (behind a reverse proxy)
	SourcesDir string
}

type Server struct {
	opt      Options
	auth     *auth.Store
	limiter  *auth.Limiter
	settings *config.SettingsStore
	runner   *runner.Runner
	store    *store.Store
	logger   *slog.Logger
}

func New(opt Options, a *auth.Store, set *config.SettingsStore, r *runner.Runner, st *store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		opt: opt, auth: a, limiter: auth.NewLimiter(5, 10*time.Minute),
		settings: set, runner: r, store: st, logger: logger,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleApp)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.opt.Version})
	})
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("POST /api/setup", s.handleSetup)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)

	a := s.requireAuth
	mux.HandleFunc("POST /api/password", a(s.handlePassword))
	mux.HandleFunc("GET /api/status", a(s.handleStatus))
	mux.HandleFunc("POST /api/run", a(s.handleRun))
	mux.HandleFunc("POST /api/stop", a(s.handleStop))
	mux.HandleFunc("POST /api/pause", a(s.handlePause))
	mux.HandleFunc("POST /api/resume", a(s.handleResume))
	mux.HandleFunc("POST /api/providers", a(s.handleAddProvider))
	mux.HandleFunc("PUT /api/providers", a(s.handleEditProvider))
	mux.HandleFunc("DELETE /api/providers", a(s.handleDeleteProvider))
	mux.HandleFunc("POST /api/providers/check", a(s.handleCheckProvider))
	mux.HandleFunc("PUT /api/active", a(s.handleSetActive))
	mux.HandleFunc("POST /api/models/test", a(s.handleTestModel))
	mux.HandleFunc("POST /api/models/draw", a(s.handleDrawTest))
	mux.HandleFunc("GET /api/events", a(s.handleEvents))
	mux.HandleFunc("GET /api/steps", a(s.handleSteps))
	mux.HandleFunc("GET /api/runs", a(s.handleRuns))
	mux.HandleFunc("DELETE /api/runs", a(s.handleDeleteRun))
	mux.HandleFunc("GET /api/image", a(s.handleImage))
	mux.HandleFunc("POST /api/pick", a(s.handlePick))
	mux.HandleFunc("POST /api/reject", a(s.handleReject))
	mux.HandleFunc("GET /api/sources", a(s.handleListDir(s.opt.SourcesDir)))
	mux.HandleFunc("POST /api/sources", a(s.handleUpload))
	mux.HandleFunc("DELETE /api/sources", a(s.handleDeleteIn(s.opt.SourcesDir)))
	mux.HandleFunc("GET /api/sources/file", a(s.handleFileIn(s.opt.SourcesDir)))
	mux.HandleFunc("GET /api/picked", a(s.handleListDir(s.store.PickedDir())))
	mux.HandleFunc("DELETE /api/picked", a(s.handleDeleteIn(s.store.PickedDir())))
	mux.HandleFunc("GET /api/picked/file", a(s.handleFileIn(s.store.PickedDir())))
	mux.HandleFunc("GET /api/picked/zip", a(s.handlePickedZip))
	mux.HandleFunc("GET /api/settings", a(s.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", a(s.handlePutSettings))

	var h http.Handler = mux
	if bp := s.opt.BasePath; bp != "" {
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == bp:
				http.Redirect(w, r, bp+"/", http.StatusMovedPermanently)
			case strings.HasPrefix(r.URL.Path, bp+"/"):
				r2 := r.Clone(r.Context())
				r2.URL.Path = strings.TrimPrefix(r.URL.Path, bp)
				r2.URL.RawPath = ""
				mux.ServeHTTP(w, r2)
			default:
				// A proxy that already stripped the prefix.
				mux.ServeHTTP(w, r)
			}
		})
	}
	return securityHeaders(h)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob: data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; frame-ancestors 'self'")
		next.ServeHTTP(w, r)
	})
}

// ---------- auth ----------

func (s *Server) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return s.opt.TrustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) clientIP(r *http.Request) string {
	if s.opt.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) cookiePath() string {
	if s.opt.BasePath == "" {
		return "/"
	}
	return s.opt.BasePath + "/"
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: s.cookiePath(),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.isHTTPS(r),
		MaxAge: int(auth.SessionTTL / time.Second),
	})
}

func (s *Server) clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: s.cookiePath(),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.isHTTPS(r), MaxAge: -1,
	})
}

func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value
	}
	return ""
}

// requireAuth accepts a panel session. Requests that change something must
// carry the X-Blendartai header, which a cross-site form cannot send.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.Valid(sessionToken(r)) {
			writeErr(w, http.StatusUnauthorized, "please log in")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) != "1" {
			writeErr(w, http.StatusForbidden, "missing "+csrfHeader+" header")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	data, err := assets.ReadFile("app.html")
	if err != nil {
		http.Error(w, "panel unavailable", http.StatusInternalServerError)
		return
	}
	html := strings.ReplaceAll(string(data), "__BASE_PATH__", s.opt.BasePath)
	html = strings.ReplaceAll(html, "__VERSION__", s.opt.Version)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, html)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	logged := s.auth.Valid(sessionToken(r))
	out := map[string]any{
		"admin_exists": s.auth.HasAdmin(),
		"logged_in":    logged,
		"version":      s.opt.Version,
	}
	if logged {
		out["username"] = s.auth.Username()
	}
	writeJSON(w, http.StatusOK, out)
}

type credsReq struct {
	Code     string `json:"code"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) limited(w http.ResponseWriter, ip string) bool {
	if blocked, wait := s.limiter.Blocked(ip); blocked {
		mins := int(wait.Minutes()) + 1
		w.Header().Set("Retry-After", itoa(int(wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, "too many tries, wait "+itoa(mins)+" min")
		return true
	}
	return false
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if s.limited(w, ip) {
		return
	}
	var req credsReq
	if !readJSON(w, r, &req) {
		return
	}
	tok, err := s.auth.Setup(req.Code, strings.TrimSpace(req.Username), req.Password)
	switch {
	case errors.Is(err, auth.ErrAdminExists):
		writeErr(w, http.StatusConflict, "setup is already done, please log in")
		return
	case errors.Is(err, auth.ErrBadSetupCode):
		s.limiter.Fail(ip)
		writeErr(w, http.StatusForbidden, "wrong setup code (see it with: sudo blendartai)")
		return
	case errors.Is(err, auth.ErrWeakPassword), errors.Is(err, auth.ErrBadUsername):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger.Info("admin created", "ip", ip)
	s.setSession(w, r, tok)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if s.limited(w, ip) {
		return
	}
	var req credsReq
	if !readJSON(w, r, &req) {
		return
	}
	tok, err := s.auth.Login(strings.TrimSpace(req.Username), req.Password)
	if errors.Is(err, auth.ErrNoAdmin) {
		writeErr(w, http.StatusConflict, "no admin yet, finish setup first")
		return
	}
	if err != nil {
		s.limiter.Fail(ip)
		s.logger.Warn("failed login", "ip", ip)
		writeErr(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	s.limiter.Reset(ip)
	s.setSession(w, r, tok)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(sessionToken(r))
	s.clearSession(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	tok, err := s.auth.ChangePassword(req.Old, req.New)
	switch {
	case errors.Is(err, auth.ErrBadLogin):
		writeErr(w, http.StatusForbidden, "current password is wrong")
		return
	case errors.Is(err, auth.ErrWeakPassword):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.setSession(w, r, tok)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- runs ----------

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.runner.Status())
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Target   int     `json:"target_images"`
		MaxUSD   float64 `json:"max_usd"`
		TextOnly *bool   `json:"text_only"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, &req) {
		return
	}
	if s.runner.Running() {
		writeErr(w, http.StatusConflict, "a run is already in progress")
		return
	}
	set := s.settings.Get()
	if req.TextOnly != nil && *req.TextOnly != set.TextOnly {
		next, err := s.settings.Update(func(c *config.Settings) error { c.TextOnly = *req.TextOnly; return nil })
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		set = next
	}
	eng, dLabel, iLabel, err := buildEngine(set)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	srcs, _ := store.ListImages(s.opt.SourcesDir)
	if len(srcs) < 2 {
		writeErr(w, http.StatusBadRequest, "upload at least 2 source images first")
		return
	}
	rc := runner.RunConfig{
		SourceDir: s.opt.SourcesDir, Target: set.TargetImages,
		PricePerImage: set.PricePerImage, MaxUSD: set.MaxUSD,
		Prompt: set.DescribePrompt, DescribeModel: dLabel, ImageModel: iLabel, ImageSize: set.ImageSize,
		TextOnly: set.TextOnly, Suffix: set.PromptSuffix,
	}
	if req.Target > 0 {
		rc.Target = min(req.Target, 10000)
	}
	if req.MaxUSD > 0 {
		rc.MaxUSD = min(req.MaxUSD, 10000)
	}
	st, err := s.runner.Start(rc, eng)
	if errors.Is(err, runner.ErrAlreadyRunning) {
		writeErr(w, http.StatusConflict, "a run is already in progress")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if err := s.runner.Pause(); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.runner.Status())
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if err := s.runner.Resume(); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.runner.Status())
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.runner.Stop()
	writeJSON(w, http.StatusOK, s.runner.Status())
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	var after int64
	for _, c := range r.URL.Query().Get("after") {
		if c < '0' || c > '9' {
			after = 0
			break
		}
		after = after*10 + int64(c-'0')
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": s.runner.Events(after)})
}

func (s *Server) handleSteps(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("run")
	if id == "" {
		id = s.runner.Status().RunID
	}
	if id == "" {
		writeJSON(w, http.StatusOK, map[string]any{"run": "", "steps": []store.StepRecord{}})
		return
	}
	steps, err := s.store.ListSteps(id)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": id, "steps": steps})
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListRuns()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if runs == nil {
		runs = []store.RunSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("run")
	if s.runner.Running() && s.runner.Status().RunID == id {
		writeErr(w, http.StatusConflict, "stop the run before deleting it")
		return
	}
	if err := s.store.DeleteRun(id); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	full, err := s.store.ResolveRunFile(r.URL.Query().Get("run"), r.URL.Query().Get("path"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !isImageName(full) {
		writeErr(w, http.StatusBadRequest, "invalid path")
		return
	}
	serveFile(w, r, full)
}

type imageActionReq struct {
	Run  string `json:"run"`
	Path string `json:"path"`
}

func (s *Server) handlePick(w http.ResponseWriter, r *http.Request) {
	var req imageActionReq
	if !readJSON(w, r, &req) {
		return
	}
	if !isImageName(req.Path) {
		writeErr(w, http.StatusBadRequest, "invalid path")
		return
	}
	if _, err := s.store.PickImage(req.Run, req.Path); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	var req imageActionReq
	if !readJSON(w, r, &req) {
		return
	}
	if !isImageName(req.Path) {
		writeErr(w, http.StatusBadRequest, "invalid path")
		return
	}
	if err := s.store.RejectImage(req.Run, req.Path); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- sources and picked ----------

func (s *Server) handleListDir(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		files, err := store.ListImages(dir)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"files": files})
	}
}

func (s *Server) handleFileIn(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := store.SafeFileIn(dir, r.URL.Query().Get("name"))
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		if r.URL.Query().Get("download") == "1" {
			w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(r.URL.Query().Get("name"), `"`, "")+`"`)
		}
		serveFile(w, r, p)
	}
}

func (s *Server) handleDeleteIn(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := store.DeleteImage(dir, r.URL.Query().Get("name")); err != nil {
			writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "expected multipart/form-data")
		return
	}
	saved := []string{}
	errs := []string{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				errs = append(errs, "upload too large")
			} else {
				errs = append(errs, err.Error())
			}
			break
		}
		if part.FileName() == "" {
			part.Close()
			continue
		}
		name, err := store.SaveSource(s.opt.SourcesDir, part.FileName(), part)
		part.Close()
		if err != nil {
			errs = append(errs, part.FileName()+": "+err.Error())
			continue
		}
		saved = append(saved, name)
	}
	status := http.StatusOK
	if len(saved) == 0 && len(errs) > 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{"saved": saved, "errors": errs})
}

func (s *Server) handlePickedZip(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="blendartai-picked-`+time.Now().Format("20060102-150405")+`.zip"`)
	if err := store.WriteZip(w, s.store.PickedDir()); err != nil {
		s.logger.Warn("zip failed", "error", err)
	}
}

// ---------- settings ----------

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, settingsView(s.settings.Get()))
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DescribePrompt *string  `json:"describe_prompt"`
		PromptSuffix   *string  `json:"prompt_suffix"`
		TextOnly       *bool    `json:"text_only"`
		ImageSize      *string  `json:"image_size"`
		TargetImages   *int     `json:"target_images"`
		PricePerImage  *float64 `json:"price_per_image"`
		MaxUSD         *float64 `json:"max_usd"`
		ProxyURL       *string  `json:"proxy_url"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	next, err := s.settings.Update(func(c *config.Settings) error {
		if req.DescribePrompt != nil {
			c.DescribePrompt = *req.DescribePrompt
		}
		if req.PromptSuffix != nil {
			c.PromptSuffix = *req.PromptSuffix
		}
		if req.TextOnly != nil {
			c.TextOnly = *req.TextOnly
		}
		if req.ImageSize != nil {
			c.ImageSize = *req.ImageSize
		}
		if req.TargetImages != nil {
			c.TargetImages = *req.TargetImages
		}
		if req.PricePerImage != nil {
			c.PricePerImage = *req.PricePerImage
		}
		if req.MaxUSD != nil {
			c.MaxUSD = *req.MaxUSD
		}
		if req.ProxyURL != nil {
			c.ProxyURL = *req.ProxyURL
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settingsView(next))
}

// ---------- helpers ----------

func isImageName(p string) bool {
	switch strings.ToLower(p[strings.LastIndex(p, ".")+1:]) {
	case "png", "jpg", "jpeg", "webp":
		return strings.Contains(p, ".")
	}
	return false
}

func serveFile(w http.ResponseWriter, r *http.Request, path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, path)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrPathTraversal):
		writeErr(w, http.StatusBadRequest, "invalid path")
	case errors.Is(err, os.ErrNotExist):
		writeErr(w, http.StatusNotFound, "not found")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
