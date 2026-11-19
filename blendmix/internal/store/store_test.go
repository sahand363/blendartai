package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestResolveRunFileRejectsTraversal(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New store: %v", err)
	}

	badCases := []struct {
		runID string
		path  string
	}{
		{"../etc", "passwd"},
		{"run1", "../other/01.png"},
		{"run1", "../../etc/passwd"},
		{"run1", "/etc/passwd"},
		{"run1", "pair/../../secret.png"},
		{"run1/sub", "01.png"},
		{"run1", ""},
		{"..", "01.png"},
	}

	for _, tc := range badCases {
		if _, err := st.ResolveRunFile(tc.runID, tc.path); !errors.Is(err, ErrPathTraversal) {
			t.Errorf("ResolveRunFile(%q, %q) = %v; want ErrPathTraversal", tc.runID, tc.path, err)
		}
	}

	got, err := st.ResolveRunFile("2026-01-01_120000", "01_a__b/01.png")
	if err != nil {
		t.Fatalf("valid path failed: %v", err)
	}
	wantSuffix := filepath.Join("runs", "2026-01-01_120000", "01_a__b", "01.png")
	if len(got) < len(wantSuffix) || got[len(got)-len(wantSuffix):] != wantSuffix {
		t.Errorf("got %q, want suffix %q", got, wantSuffix)
	}
}
