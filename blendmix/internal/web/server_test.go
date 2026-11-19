package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sahand363/blendartai/blendmix/internal/config"
	"github.com/sahand363/blendartai/blendmix/internal/runner"
	"github.com/sahand363/blendartai/blendmix/internal/store"
)

func TestCheckBearerToken(t *testing.T) {
	if !CheckBearerToken("Bearer secret-123", "secret-123") {
		t.Fatal("expected valid token to pass")
	}
	bad := []string{
		"",
		"secret-123",
		"Basic secret-123",
		"Bearer wrong",
		"Bearer secret-1234",
	}
	for _, h := range bad {
		if CheckBearerToken(h, "secret-123") {
			t.Errorf("expected %q to be rejected", h)
		}
	}
}

func TestAPIAuthAndTraversalRejection(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Token:    "topsecret",
		BasePath: "/plugins/mixer",
	}
	r := runner.New(cfg, nil, st, nil)
	srv := NewServer(cfg, r, st, nil)
	h := srv.Handler()

	// Missing token -> 401
	req := httptest.NewRequest(http.MethodGet, "/plugins/mixer/api/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d, want 401", rec.Code)
	}

	// Valid token -> 200
	req = httptest.NewRequest(http.MethodGet, "/plugins/mixer/api/status", nil)
	req.Header.Set("Authorization", "Bearer topsecret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token: got %d, want 200", rec.Code)
	}

	// Path traversal on /api/image -> 400
	req = httptest.NewRequest(http.MethodGet, "/plugins/mixer/api/image?run=r1&path=../../etc/passwd", nil)
	req.Header.Set("Authorization", "Bearer topsecret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("path traversal: got %d, want 400", rec.Code)
	}
}
