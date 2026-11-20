package store

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MaxSourceBytes caps one uploaded source image.
const MaxSourceBytes = 20 << 20

type FileInfo struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// PickedDir is where kept images are copied.
func (s *Store) PickedDir() string { return s.pickedDir }

// ListImages lists image files (not folders) in dir, newest first.
func ListImages(dir string) ([]FileInfo, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []FileInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []FileInfo{}
	for _, e := range entries {
		if e.IsDir() || !isImageExt(e.Name()) || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, FileInfo{Name: e.Name(), Size: fi.Size(), ModTime: fi.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModTime.Equal(out[j].ModTime) {
			return out[i].Name < out[j].Name
		}
		return out[i].ModTime.After(out[j].ModTime)
	})
	return out, nil
}

// SafeFileIn resolves a plain file name inside dir. Names with any path
// separator, "..", or a non-image extension are rejected.
func SafeFileIn(dir, name string) (string, error) {
	if !isSafeSegment(name) || strings.HasPrefix(name, ".") || !isImageExt(name) {
		return "", ErrPathTraversal
	}
	return filepath.Join(dir, name), nil
}

// SaveSource stores an uploaded image in dir under a cleaned, unique name.
// The content must look like PNG, JPEG or WebP.
func SaveSource(dir, origName string, r io.Reader) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxSourceBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > MaxSourceBytes {
		return "", fmt.Errorf("file is larger than %d MB", MaxSourceBytes>>20)
	}
	ext := sniffExt(data)
	if ext == "" {
		return "", errors.New("not a PNG, JPEG or WebP image")
	}
	base := sanitizeName(origName)
	name := base + ext
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(dir, name)); errors.Is(err, os.ErrNotExist) {
			break
		}
		name = fmt.Sprintf("%s-%d%s", base, i, ext)
	}
	if err := WriteFileAtomic(filepath.Join(dir, name), data, 0o644); err != nil {
		return "", err
	}
	return name, nil
}

func sniffExt(b []byte) string {
	switch {
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return ".png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return ".jpg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return ".webp"
	}
	return ""
}

// DeleteImage removes an image and its JSON sidecar from dir.
func DeleteImage(dir, name string) error {
	p, err := SafeFileIn(dir, name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	_ = os.Remove(strings.TrimSuffix(p, filepath.Ext(p)) + ".json")
	return nil
}

// WriteZip streams every image in dir (with sidecars) as a zip archive.
func WriteZip(w io.Writer, dir string) error {
	files, err := ListImages(dir)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	for _, f := range files {
		for _, name := range []string{f.Name, strings.TrimSuffix(f.Name, filepath.Ext(f.Name)) + ".json"} {
			src, err := os.Open(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			dst, err := zw.Create(name)
			if err == nil {
				_, err = io.Copy(dst, src)
			}
			src.Close()
			if err != nil {
				return err
			}
		}
	}
	return zw.Close()
}

// DeleteRun removes a finished run folder.
func (s *Store) DeleteRun(runID string) error {
	if !isSafeSegment(runID) {
		return ErrPathTraversal
	}
	dir := filepath.Join(s.runsDir, runID)
	if _, err := os.Stat(dir); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
