package auth

import (
	"testing"
	"time"
)

func TestSetupLoginFlow(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	code, err := a.EnsureSetupCode()
	if err != nil || len(code) < 8 {
		t.Fatalf("code %q %v", code, err)
	}
	if again, _ := a.EnsureSetupCode(); again != code {
		t.Fatal("setup code should be stable until used")
	}
	if _, err := a.Setup("WRONGCODE1", "admin", "longpassword1"); err != ErrBadSetupCode {
		t.Fatalf("want bad code, got %v", err)
	}
	if _, err := a.Setup(code, "admin", "short"); err != ErrWeakPassword {
		t.Fatalf("want weak, got %v", err)
	}
	tok, err := a.Setup(code, "admin", "longpassword1")
	if err != nil || !a.Valid(tok) {
		t.Fatalf("setup %v", err)
	}
	if _, err := a.Setup(code, "admin2", "longpassword1"); err == nil {
		t.Fatal("second setup must fail")
	}
	if c, _ := a.EnsureSetupCode(); c != "" {
		t.Fatal("no setup code once admin exists")
	}
	if _, err := a.Login("admin", "nope-nope-nope"); err != ErrBadLogin {
		t.Fatalf("want bad login, got %v", err)
	}
	t2, err := a.Login("admin", "longpassword1")
	if err != nil {
		t.Fatal(err)
	}
	a.Logout(t2)
	if a.Valid(t2) {
		t.Fatal("logged-out token still valid")
	}
	if err := a.Reset("admin", "anotherpass12"); err != nil {
		t.Fatal(err)
	}
	if a.Valid(tok) {
		t.Fatal("reset must drop sessions")
	}
	b, _ := Open(dir)
	if _, err := b.Login("admin", "anotherpass12"); err != nil {
		t.Fatalf("reload: %v", err)
	}
}

func TestAPIToken(t *testing.T) {
	tok := NewAPIToken()
	h := HashToken(tok)
	if !TokenMatches(tok, h) || TokenMatches(tok+"x", h) || TokenMatches(tok, "") {
		t.Fatal("token match broken")
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if b, _ := l.Blocked("ip"); b {
			t.Fatal("blocked too early")
		}
		l.Fail("ip")
	}
	if b, _ := l.Blocked("ip"); !b {
		t.Fatal("should block after 3 fails")
	}
	if b, _ := l.Blocked("other"); b {
		t.Fatal("other ip blocked")
	}
	l.Reset("ip")
	if b, _ := l.Blocked("ip"); b {
		t.Fatal("reset failed")
	}
}
