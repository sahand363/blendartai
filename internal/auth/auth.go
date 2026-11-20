// Package auth keeps the single admin login, its sessions, the one-time
// setup code and a login rate limiter. Everything lives in <data>/auth.json
// (0600) and is re-read when the file changes, so a password reset from the
// SSH menu applies to the running server at once.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	MinPasswordLen = 10
	SessionTTL     = 30 * 24 * time.Hour
)

var (
	ErrNoAdmin        = errors.New("no admin yet")
	ErrAdminExists    = errors.New("admin already exists")
	ErrBadSetupCode   = errors.New("wrong setup code")
	ErrBadLogin       = errors.New("wrong username or password")
	ErrWeakPassword   = errors.New("password must be at least 10 characters")
	ErrBadUsername    = errors.New("username must be 1 to 32 letters, digits, dot, dash or underscore")
	ErrSessionInvalid = errors.New("session invalid")
)

type fileData struct {
	Username     string           `json:"username"`
	PasswordHash string           `json:"password_hash"`
	Sessions     map[string]int64 `json:"sessions"` // sha256(token) -> unix expiry
}

type Store struct {
	dir  string
	path string
	mu   sync.Mutex
	d    fileData
	mod  int64
	now  func() time.Time
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dataDir, path: filepath.Join(dataDir, "auth.json"), now: time.Now}
	s.d.Sessions = map[string]int64{}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) reloadLocked() error {
	fi, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.d = fileData{Sessions: map[string]int64{}}
		s.mod = 0
		return nil
	}
	if err != nil {
		return err
	}
	if fi.ModTime().UnixNano() == s.mod {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var d fileData
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	if d.Sessions == nil {
		d.Sessions = map[string]int64{}
	}
	s.d = d
	s.mod = fi.ModTime().UnixNano()
	return nil
}

func (s *Store) saveLocked() error {
	now := s.now().Unix()
	for k, exp := range s.d.Sessions {
		if exp < now {
			delete(s.d.Sessions, k)
		}
	}
	raw, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".auth-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return err
	}
	if fi, err := os.Stat(s.path); err == nil {
		s.mod = fi.ModTime().UnixNano()
	}
	return nil
}

func (s *Store) HasAdmin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadLocked()
	return s.d.Username != "" && s.d.PasswordHash != ""
}

func (s *Store) Username() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadLocked()
	return s.d.Username
}

// --- setup code ---

func (s *Store) setupCodePath() string { return filepath.Join(s.dir, "setup_code") }

// EnsureSetupCode returns the one-time setup code, creating it when no admin
// exists. With an admin it removes any old code and returns "".
func (s *Store) EnsureSetupCode() (string, error) {
	if s.HasAdmin() {
		_ = os.Remove(s.setupCodePath())
		return "", nil
	}
	if raw, err := os.ReadFile(s.setupCodePath()); err == nil {
		if c := strings.TrimSpace(string(raw)); len(c) >= 8 {
			return c, nil
		}
	}
	code := randomCode(10)
	if err := os.WriteFile(s.setupCodePath(), []byte(code+"\n"), 0o600); err != nil {
		return "", err
	}
	return code, nil
}

func (s *Store) checkSetupCode(code string) bool {
	raw, err := os.ReadFile(s.setupCodePath())
	if err != nil {
		return false
	}
	want := strings.TrimSpace(string(raw))
	got := strings.ToUpper(strings.TrimSpace(code))
	return want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// Setup creates the admin. It only works while no admin exists and with the
// setup code printed by the installer / shown in the SSH menu.
func (s *Store) Setup(code, username, password string) (string, error) {
	if err := validUsername(username); err != nil {
		return "", err
	}
	if len(password) < MinPasswordLen {
		return "", ErrWeakPassword
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadLocked()
	if s.d.Username != "" && s.d.PasswordHash != "" {
		return "", ErrAdminExists
	}
	if !s.checkSetupCode(code) {
		return "", ErrBadSetupCode
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	s.d.Username = username
	s.d.PasswordHash = string(hash)
	s.d.Sessions = map[string]int64{}
	tok := s.newSessionLocked()
	if err := s.saveLocked(); err != nil {
		return "", err
	}
	_ = os.Remove(s.setupCodePath())
	return tok, nil
}

// --- login / sessions ---

func (s *Store) Login(username, password string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadLocked()
	if s.d.PasswordHash == "" {
		return "", ErrNoAdmin
	}
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(s.d.Username)) == 1
	pwOK := bcrypt.CompareHashAndPassword([]byte(s.d.PasswordHash), []byte(password)) == nil
	if !userOK || !pwOK {
		return "", ErrBadLogin
	}
	tok := s.newSessionLocked()
	if err := s.saveLocked(); err != nil {
		return "", err
	}
	return tok, nil
}

func (s *Store) newSessionLocked() string {
	tok := randomHex(32)
	s.d.Sessions[hashToken(tok)] = s.now().Add(SessionTTL).Unix()
	return tok
}

func (s *Store) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadLocked()
	exp, ok := s.d.Sessions[hashToken(token)]
	return ok && exp > s.now().Unix()
}

func (s *Store) Logout(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadLocked()
	if _, ok := s.d.Sessions[hashToken(token)]; ok {
		delete(s.d.Sessions, hashToken(token))
		_ = s.saveLocked()
	}
}

// ChangePassword checks the old password, sets the new one and logs out
// every other session. It returns a fresh session for the caller.
func (s *Store) ChangePassword(oldPW, newPW string) (string, error) {
	if len(newPW) < MinPasswordLen {
		return "", ErrWeakPassword
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadLocked()
	if bcrypt.CompareHashAndPassword([]byte(s.d.PasswordHash), []byte(oldPW)) != nil {
		return "", ErrBadLogin
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPW), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	s.d.PasswordHash = string(hash)
	s.d.Sessions = map[string]int64{}
	tok := s.newSessionLocked()
	return tok, s.saveLocked()
}

// Reset sets the admin username and password without the old password (SSH
// menu / CLI) and logs out every session.
func (s *Store) Reset(username, password string) error {
	if err := validUsername(username); err != nil {
		return err
	}
	if len(password) < MinPasswordLen {
		return ErrWeakPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadLocked()
	s.d.Username = username
	s.d.PasswordHash = string(hash)
	s.d.Sessions = map[string]int64{}
	if err := s.saveLocked(); err != nil {
		return err
	}
	_ = os.Remove(s.setupCodePath())
	return nil
}

func validUsername(u string) error {
	if len(u) < 1 || len(u) > 32 {
		return ErrBadUsername
	}
	for _, r := range u {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_'
		if !ok {
			return ErrBadUsername
		}
	}
	return nil
}

// --- helpers ---

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// HashToken is exported for the API token, which is stored the same way.
func HashToken(tok string) string { return hashToken(tok) }

// TokenMatches compares a presented token with a stored sha256 hex hash in
// constant time.
func TokenMatches(token, storedHash string) bool {
	if token == "" || storedHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(storedHash)) == 1
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// NewAPIToken returns a fresh random API token.
func NewAPIToken() string { return "bat_" + randomHex(24) }

func randomCode(n int) string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/I
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// --- rate limiter ---

// Limiter allows Max failures per Window for each key (client IP).
type Limiter struct {
	Max    int
	Window time.Duration
	mu     sync.Mutex
	fails  map[string][]time.Time
	now    func() time.Time
}

func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{Max: max, Window: window, fails: map[string][]time.Time{}, now: time.Now}
}

func (l *Limiter) prune(key string) []time.Time {
	cut := l.now().Add(-l.Window)
	in := l.fails[key]
	out := in[:0]
	for _, t := range in {
		if t.After(cut) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = out
	return out
}

// Blocked reports whether key is locked out, and for how long.
func (l *Limiter) Blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.prune(key)
	if len(f) < l.Max {
		return false, 0
	}
	return true, f[0].Add(l.Window).Sub(l.now())
}

func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(key)
	l.fails[key] = append(l.fails[key], l.now())
	if len(l.fails) > 10000 { // keep memory bounded
		for k := range l.fails {
			delete(l.fails, k)
			break
		}
	}
}

func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}
