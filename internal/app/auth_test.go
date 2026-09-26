package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func authServer(t *testing.T, password, dir string) http.Handler {
	t.Helper()
	a, err := newAuth(password, dir)
	if err != nil {
		t.Fatal(err)
	}
	return (&server{live: NewLive(defaultAlerts()), auth: a}).mux(newQueue(1))
}

func login(t *testing.T, h http.Handler, password string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(url.Values{"password": {password}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func get(h http.Handler, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func sessionFrom(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie set")
	return nil
}

// Without a password the server is exactly as open as it always was.
func TestNoPasswordLeavesTheUIOpen(t *testing.T) {
	a, err := newAuth("", t.TempDir())
	if a != nil || err != nil {
		t.Fatalf("newAuth(\"\") = %v, %v; want nil, nil", a, err)
	}
	h := (&server{live: NewLive(defaultAlerts())}).mux(newQueue(1))
	if w := get(h, "/"); w.Code != http.StatusOK {
		t.Fatalf("/ = %d, want 200", w.Code)
	}
}

func TestSignedOutVisitorsReachOnlyTheLoginPage(t *testing.T) {
	h := authServer(t, "hunter2", t.TempDir())
	for _, path := range []string{"/", "/settings"} {
		if w := get(h, path); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
			t.Errorf("%s = %d to %q, want a redirect to /login", path, w.Code, w.Header().Get("Location"))
		}
	}
	if w := get(h, "/api/alerts"); w.Code != http.StatusUnauthorized {
		t.Errorf("/api/alerts = %d, want 401", w.Code)
	}
	if w := get(h, "/api/config/export"); w.Code != http.StatusUnauthorized {
		t.Errorf("/api/config/export = %d, want 401", w.Code)
	}
	if w := get(h, "/login"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `name="password"`) {
		t.Errorf("/login = %d, want the form", w.Code)
	}
	if w := get(h, "/public-sans.woff2"); w.Code != http.StatusOK {
		t.Errorf("font = %d, want 200 for the login page to use", w.Code)
	}
}

// The container HEALTHCHECK probes /healthz with no cookie; it must not be sent to
// the login page.
func TestHealthzNeedsNoSession(t *testing.T) {
	a, err := newAuth("hunter2", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := a.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if w := get(h, "/healthz"); w.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want it served without a session", w.Code)
	}
}

func TestWrongPasswordIsRefused(t *testing.T) {
	h := authServer(t, "hunter2", t.TempDir())
	w := login(t, h, "hunter3")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "not right") {
		t.Fatalf("wrong password = %d, want 401 with the error shown", w.Code)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("a wrong password was given a cookie")
	}
	// Straight after a failure even the right password is turned away unchecked.
	if w := login(t, h, "hunter2"); w.Code != http.StatusTooManyRequests || len(w.Result().Cookies()) != 0 {
		t.Fatalf("guess inside the lockout = %d, want 429 and no cookie", w.Code)
	}
}

// A burst of simultaneous guesses from one client must not all be tested: the lockout
// is taken before a guess is checked, so only one of them can be.
func TestConcurrentGuessesAreCheckedOneAtATime(t *testing.T) {
	a, err := newAuth("hunter2", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if checked := guessAtOnce(a, 50, func(int) string { return "192.0.2.9" }); checked != 1 {
		t.Fatalf("%d of 50 concurrent guesses were checked, want 1", checked)
	}
}

// Many addresses guessing together are held to the overall cap.
func TestGuessesFromManyClientsAreCapped(t *testing.T) {
	a, err := newAuth("hunter2", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checked := guessAtOnce(a, 100, func(i int) string { return fmt.Sprintf("198.51.100.%d", i) })
	if checked != loginFailsPerSecond {
		t.Fatalf("%d of 100 guesses from distinct clients were checked, want %d", checked, loginFailsPerSecond)
	}
}

// guessAtOnce fires n wrong guesses concurrently and reports how many were checked.
func guessAtOnce(a *auth, n int, client func(int) string) int {
	results := make(chan loginResult, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); results <- a.checkPassword(client(i), fmt.Sprint("guess", i)) }()
	}
	wg.Wait()
	close(results)
	checked := 0
	for r := range results {
		if r != loginThrottled {
			checked++
		}
	}
	return checked
}

// A stranger guessing wrong must not lock the operator out.
func TestOneClientsFailuresDoNotLockOutAnother(t *testing.T) {
	h := authServer(t, "hunter2", t.TempDir())
	attempt := func(addr, pw string) int {
		req := httptest.NewRequest(http.MethodPost, "/login",
			strings.NewReader(url.Values{"password": {pw}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if code := attempt("203.0.113.5:4000", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("attacker's guess = %d, want 401", code)
	}
	if code := attempt("192.168.1.20:5000", "hunter2"); code != http.StatusSeeOther {
		t.Fatalf("operator's login during the attacker's lockout = %d, want 303", code)
	}
	// IPv6 is throttled per /64, so rotating the interface ID does not reset it.
	if code := attempt("[2001:db8:1:2::a]:4000", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("first IPv6 guess = %d, want 401", code)
	}
	if code := attempt("[2001:db8:1:2::b]:4000", "wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("guess from the same /64 = %d, want 429", code)
	}
}

// Another site's page must not be able to change settings through the viewer's
// browser, whether or not the UI has a password.
func TestCrossSiteWritesAreRefused(t *testing.T) {
	dir := t.TempDir()
	c := sessionFrom(t, login(t, authServer(t, "hunter2", dir), "hunter2"))
	for name, h := range map[string]http.Handler{
		"signed in": authServer(t, "hunter2", dir),
		"open":      (&server{live: NewLive(defaultAlerts())}).mux(newQueue(1)),
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/config/import", strings.NewReader(`{}`))
		req.Header.Set("Sec-Fetch-Site", "same-site")
		req.AddCookie(c)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: same-site import = %d, want 403", name, w.Code)
		}
	}
}

func TestLoginOpensTheUIAndLogoutClosesIt(t *testing.T) {
	h := authServer(t, "hunter2", t.TempDir())
	w := login(t, h, "hunter2")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("login = %d to %q, want a redirect to /", w.Code, w.Header().Get("Location"))
	}
	c := sessionFrom(t, w)
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie HttpOnly=%v SameSite=%v", c.HttpOnly, c.SameSite)
	}
	if w := get(h, "/", c); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="brand"`) {
		t.Fatalf("/ with a session = %d, want the app", w.Code)
	}
	if w := get(h, "/login", c); w.Code != http.StatusSeeOther {
		t.Errorf("/login when signed in = %d, want a redirect to the app", w.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(c)
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if cleared := sessionFrom(t, out); cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Errorf("logout cookie = %+v, want it deleted", cleared)
	}
}

func TestForgedAndExpiredSessionsAreRefused(t *testing.T) {
	dir := t.TempDir()
	h := authServer(t, "hunter2", dir)
	a, _ := newAuth("hunter2", dir)

	expired := &http.Cookie{Name: sessionCookie, Value: a.token(time.Now().Add(-time.Minute))}
	forged := &http.Cookie{Name: sessionCookie, Value: "99999999999.AAAA"}
	for name, c := range map[string]*http.Cookie{"expired": expired, "forged": forged} {
		if w := get(h, "/api/alerts", c); w.Code != http.StatusUnauthorized {
			t.Errorf("%s session = %d, want 401", name, w.Code)
		}
	}
}

// The signing secret is kept on disk, so a restart keeps people signed in; the
// password is part of the key, so changing it signs everyone out.
func TestSessionsSurviveARestartButNotAPasswordChange(t *testing.T) {
	dir := t.TempDir()
	c := sessionFrom(t, login(t, authServer(t, "hunter2", dir), "hunter2"))

	if w := get(authServer(t, "hunter2", dir), "/", c); w.Code != http.StatusOK {
		t.Errorf("after restart = %d, want still signed in", w.Code)
	}
	if w := get(authServer(t, "correct horse", dir), "/", c); w.Code != http.StatusSeeOther {
		t.Errorf("after password change = %d, want signed out", w.Code)
	}
}

// Startup refuses SKY_ variables it does not know, so the password must be one it does.
func TestUIPasswordIsAKnownVariable(t *testing.T) {
	if err := checkEnv(map[string]string{uiPasswordEnv: "hunter2"}); err != nil {
		t.Fatal(err)
	}
}
