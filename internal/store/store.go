package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var ErrPathTraversal = errors.New("invalid path")

type RunState struct {
	RunID         string    `json:"run_id"`
	Status        string    `json:"status"`
	ImagesDone    int       `json:"images_done"`
	PairsDone     int       `json:"pairs_done"`
	EstSpendUSD   float64   `json:"est_spend_usd"`
	MaxImages     int       `json:"max_images,omitempty"`
	MaxUSD        float64   `json:"max_usd,omitempty"`
	Steps         int       `json:"steps"`
	Descriptions  int       `json:"descriptions"`
	Errors        int       `json:"errors"`
	DescribeModel string    `json:"describe_model,omitempty"`
	ImageModel    string    `json:"image_model,omitempty"`
	CurrentPair   string    `json:"current_pair,omitempty"`
	CurrentStep   int       `json:"current_step,omitempty"`
	Phase         string    `json:"phase,omitempty"` // loading, describing, drawing, saving, waiting, paused
	PhaseDetail   string    `json:"phase_detail,omitempty"`
	PhaseSince    time.Time `json:"phase_since,omitzero"`
	ContentImage  string    `json:"content_image,omitempty"`
	StyleImage    string    `json:"style_image,omitempty"`
	ContentThumb  string    `json:"content_thumb,omitempty"`
	StyleThumb    string    `json:"style_thumb,omitempty"`
	StopReason    string    `json:"stop_reason,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type ImageMeta struct {
	SourceFiles   []string  `json:"source_files"`
	ContentImage  string    `json:"content_image,omitempty"`
	StyleImage    string    `json:"style_image,omitempty"`
	DescribeModel string    `json:"describe_model,omitempty"`
	Prompt        string    `json:"prompt"`
	Model         string    `json:"model"`
	Size          string    `json:"size"`
	Timestamp     time.Time `json:"timestamp"`
	EstCostUSD    float64   `json:"est_cost_usd"`
}

type GalleryImage struct {
	Path   string    `json:"path"`
	Name   string    `json:"name"`
	Prompt string    `json:"prompt"`
	Meta   ImageMeta `json:"meta"`
}

type GalleryPair struct {
	Name   string         `json:"name"`
	Images []GalleryImage `json:"images"`
}

type RunSummary struct {
	RunID string        `json:"run_id"`
	State RunState      `json:"state"`
	Pairs []GalleryPair `json:"pairs"`
}

type Store struct {
	outDir    string
	runsDir   string
	pickedDir string
}

func New(outDir string) (*Store, error) {
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		return nil, err
	}
	runsDir := filepath.Join(absOut, "runs")
	pickedDir := filepath.Join(absOut, "picked")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(pickedDir, 0o755); err != nil {
		return nil, err
	}
	return &Store{
		outDir:    absOut,
		runsDir:   runsDir,
		pickedDir: pickedDir,
	}, nil
}

func (s *Store) CreateRun(runID string) (string, error) {
	if !isSafeSegment(runID) {
		return "", ErrPathTraversal
	}
	dir := filepath.Join(s.runsDir, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func (s *Store) PairDir(runID string, pairIndex int, srcA, srcB string) (string, error) {
	if !isSafeSegment(runID) {
		return "", ErrPathTraversal
	}
	name := StepDirName(pairIndex, srcA, srcB)
	dir := filepath.Join(s.runsDir, runID, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func (s *Store) WriteRunState(runID string, st RunState) error {
	if !isSafeSegment(runID) {
		return ErrPathTraversal
	}
	st.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(s.runsDir, runID, "run.json"), data, 0o644)
}

func (s *Store) WriteDescribe(pairDir string, raw []byte) error {
	return WriteFileAtomic(filepath.Join(pairDir, "describe.json"), raw, 0o644)
}

// WriteImageAndMeta saves NN.<ext> and NN.json in pairDir and returns the
// image file name.
func (s *Store) WriteImageAndMeta(pairDir string, idx int, imgBytes []byte, mime string, meta ImageMeta) (string, error) {
	base := fmt.Sprintf("%02d", idx)
	ext := ".png"
	switch strings.ToLower(mime) {
	case "image/jpeg", "image/jpg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	}
	if err := WriteFileAtomic(filepath.Join(pairDir, base+ext), imgBytes, 0o644); err != nil {
		return "", err
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return "", err
	}
	return base + ext, WriteFileAtomic(filepath.Join(pairDir, base+".json"), metaBytes, 0o644)
}

// ResolveRunFile safely resolves a relative path inside runs/<runID>/,
// rejecting any path traversal attempts.
func (s *Store) ResolveRunFile(runID, relPath string) (string, error) {
	if !isSafeSegment(runID) {
		return "", ErrPathTraversal
	}
	if relPath == "" || strings.Contains(relPath, "\x00") {
		return "", ErrPathTraversal
	}
	if filepath.IsAbs(relPath) || strings.HasPrefix(relPath, "/") || strings.HasPrefix(relPath, "\\") {
		return "", ErrPathTraversal
	}

	runRoot := filepath.Join(s.runsDir, runID)
	cleanRel := filepath.Clean(filepath.FromSlash(relPath))
	if cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(filepath.Separator)) {
		return "", ErrPathTraversal
	}

	full := filepath.Join(runRoot, cleanRel)
	relCheck, err := filepath.Rel(runRoot, full)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", ErrPathTraversal
	}
	return full, nil
}

func (s *Store) PickImage(runID, relPath string) (string, error) {
	src, err := s.ResolveRunFile(runID, relPath)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(src); err != nil {
		return "", err
	}

	flatName := fmt.Sprintf("%s__%s", runID, strings.ReplaceAll(filepath.ToSlash(relPath), "/", "__"))
	dst := filepath.Join(s.pickedDir, flatName)
	if err := copyFileAtomic(src, dst); err != nil {
		return "", err
	}

	// Copy sidecar JSON if present.
	metaSrc := strings.TrimSuffix(src, filepath.Ext(src)) + ".json"
	if _, err := os.Stat(metaSrc); err == nil {
		metaDst := strings.TrimSuffix(dst, filepath.Ext(dst)) + ".json"
		_ = copyFileAtomic(metaSrc, metaDst)
	}
	return dst, nil
}

func (s *Store) RejectImage(runID, relPath string) error {
	src, err := s.ResolveRunFile(runID, relPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(src); err != nil {
		return err
	}

	rejDir := filepath.Join(s.runsDir, runID, "rejected", filepath.Dir(filepath.FromSlash(relPath)))
	if err := os.MkdirAll(rejDir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(rejDir, filepath.Base(src))
	if err := os.Rename(src, dst); err != nil {
		return err
	}

	metaSrc := strings.TrimSuffix(src, filepath.Ext(src)) + ".json"
	if _, err := os.Stat(metaSrc); err == nil {
		metaDst := filepath.Join(rejDir, filepath.Base(metaSrc))
		_ = os.Rename(metaSrc, metaDst)
	}
	return nil
}

func (s *Store) ListRuns() ([]RunSummary, error) {
	entries, err := os.ReadDir(s.runsDir)
	if err != nil {
		return nil, err
	}

	var runs []RunSummary
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		runID := e.Name()
		runPath := filepath.Join(s.runsDir, runID)

		var st RunState
		st.RunID = runID
		if raw, err := os.ReadFile(filepath.Join(runPath, "run.json")); err == nil {
			_ = json.Unmarshal(raw, &st)
		}

		pairs := listPairs(runPath)
		runs = append(runs, RunSummary{
			RunID: runID,
			State: st,
			Pairs: pairs,
		})
	}

	sort.Slice(runs, func(i, j int) bool {
		return runs[i].RunID > runs[j].RunID
	})
	return runs, nil
}

func listPairs(runPath string) []GalleryPair {
	entries, err := os.ReadDir(runPath)
	if err != nil {
		return nil
	}
	var pairs []GalleryPair
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "rejected" || e.Name() == "sources" {
			continue
		}
		pairName := e.Name()
		pairPath := filepath.Join(runPath, pairName)
		files, err := os.ReadDir(pairPath)
		if err != nil {
			continue
		}
		var imgs []GalleryImage
		for _, f := range files {
			if f.IsDir() || !isImageExt(f.Name()) {
				continue
			}
			rel := filepath.ToSlash(filepath.Join(pairName, f.Name()))
			var meta ImageMeta
			metaPath := filepath.Join(pairPath, strings.TrimSuffix(f.Name(), filepath.Ext(f.Name()))+".json")
			if raw, err := os.ReadFile(metaPath); err == nil {
				_ = json.Unmarshal(raw, &meta)
			}
			imgs = append(imgs, GalleryImage{
				Path:   rel,
				Name:   f.Name(),
				Prompt: meta.Prompt,
				Meta:   meta,
			})
		}
		if len(imgs) > 0 {
			sort.Slice(imgs, func(i, j int) bool { return imgs[i].Name < imgs[j].Name })
			pairs = append(pairs, GalleryPair{Name: pairName, Images: imgs})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Name < pairs[j].Name })
	return pairs
}

// WriteFileAtomic writes data to a temp file in the same directory, syncs it,
// and atomically renames it into place.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	data, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	return WriteFileAtomic(dst, data, 0o644)
}

func isSafeSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, "/\\\x00")
}

func sanitizeName(filename string) string {
	base := strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	var b strings.Builder
	for _, r := range base {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "img"
	}
	return out
}

func isImageExt(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp":
		return true
	}
	return false
}
