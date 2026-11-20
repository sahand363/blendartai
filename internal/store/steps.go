package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// StepRecord is one step of a run: the two source images, the description
// the describe model wrote, and the picture the image model made from it.
// It is saved as step.json in the step's folder and updated as it goes.
type StepRecord struct {
	Step          int       `json:"step"`
	Dir           string    `json:"dir"`           // step folder, relative to the run
	Content       string    `json:"content"`       // source file name (content)
	Style         string    `json:"style"`         // source file name (style)
	ContentThumb  string    `json:"content_thumb"` // copy inside the run, relative path
	StyleThumb    string    `json:"style_thumb"`   // copy inside the run, relative path
	Status        string    `json:"status"`        // describing, drawing, done, failed
	Description   string    `json:"description,omitempty"`
	Image         string    `json:"image,omitempty"` // relative path of the made image
	Error         string    `json:"error,omitempty"`
	DescribeModel string    `json:"describe_model,omitempty"`
	ImageModel    string    `json:"image_model,omitempty"`
	DescribeMS    int64     `json:"describe_ms,omitempty"`
	DrawMS        int64     `json:"draw_ms,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at,omitzero"`
}

// WriteStep saves rec as <run>/<rec.Dir>/step.json.
func (s *Store) WriteStep(runID string, rec StepRecord) error {
	if !isSafeSegment(runID) || !isSafeSegment(rec.Dir) {
		return ErrPathTraversal
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(s.runsDir, runID, rec.Dir, "step.json"), data, 0o644)
}

// ListSteps returns every step of a run, newest first.
func (s *Store) ListSteps(runID string) ([]StepRecord, error) {
	if !isSafeSegment(runID) {
		return nil, ErrPathTraversal
	}
	root := filepath.Join(s.runsDir, runID)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := []StepRecord{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, e.Name(), "step.json"))
		if err != nil {
			continue
		}
		var rec StepRecord
		if json.Unmarshal(raw, &rec) != nil {
			continue
		}
		rec.Dir = e.Name()
		// The image may have been rejected (moved) since.
		if rec.Image != "" {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rec.Image))); err != nil {
				rec.Image = ""
				if rec.Status == "done" {
					rec.Status = "rejected"
				}
			}
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Step > out[j].Step })
	return out, nil
}

// DeleteStep removes one step folder (its description, and its image if
// one was made). Picked copies are kept.
func (s *Store) DeleteStep(runID, dir string) error {
	if !isSafeSegment(runID) || !isSafeSegment(dir) || dir == "sources" || dir == "rejected" {
		return ErrPathTraversal
	}
	full := filepath.Join(s.runsDir, runID, dir)
	if _, err := os.Stat(filepath.Join(full, "step.json")); err != nil {
		return os.ErrNotExist
	}
	return os.RemoveAll(full)
}

// SaveRunSource keeps a copy of a (shrunk) source image inside the run, so
// the run still shows which images it mixed after the sources change.
// It returns the path relative to the run.
func (s *Store) SaveRunSource(runID, name string, data []byte, mime string) (string, error) {
	if !isSafeSegment(runID) {
		return "", ErrPathTraversal
	}
	ext := ".jpg"
	if strings.EqualFold(mime, "image/png") {
		ext = ".png"
	}
	rel := "sources/" + sanitizeName(name) + "_" + fmt.Sprintf("%x", hashName(name)) + ext
	full := filepath.Join(s.runsDir, runID, filepath.FromSlash(rel))
	if _, err := os.Stat(full); err == nil {
		return rel, nil
	}
	if err := WriteFileAtomic(full, data, 0o644); err != nil {
		return "", err
	}
	return rel, nil
}

func hashName(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// AppendEvent adds one line to the run's events.log.
func (s *Store) AppendEvent(runID string, line string) {
	if !isSafeSegment(runID) {
		return
	}
	f, err := os.OpenFile(filepath.Join(s.runsDir, runID, "events.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line + "\n")
	_ = f.Close()
}

// StepDirName is the folder name for a step.
func StepDirName(step int, content, style string) string {
	return fmt.Sprintf("%03d_%s__%s", step, sanitizeName(content), sanitizeName(style))
}
