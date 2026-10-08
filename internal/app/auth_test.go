package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// authServer serves the UI behind password, given as SKY_UI_PASSWORD would give it.
func authServer(t *testing.T, password string, store *SettingsStore) http.Handler {
	t.Helper()
	a, err := newAuth(password, store)
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

// Choosing to go without a password leaves the server exactly as open as it always was.
func TestNoPasswordLeavesTheUIOpen(t *testing.T) {
	store := newTestStore(t)
	if err := store.SetUIPassword(""); err != nil {
		t.Fatal(err)
	}
	h := authServer(t, "", store)
	if w := get(h, "/"); w.Code != http.StatusOK {
		t.Fatalf("/ = %d, want 200", w.Code)
	}
	if w := get(h, "/login"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("/login = %d to %q, want a redirect to the app", w.Code, w.Header().Get("Location"))
	}
}

func TestSignedOutVisitorsReachOnlyTheLoginPage(t *testing.T) {
	h := authServer(t, "hunter2", newTestStore(t))
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
	a, err := newAuth("hunter2", newTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	h := a.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if w := get(h, "/healthz"); w.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want it served without a session", w.Code)
	}
}

func TestWrongPasswordIsRefused(t *testing.T) {
	h := authServer(t, "hunter2", newTestStore(t))
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
	a, err := newAuth("hunter2", newTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	if checked := guessAtOnce(a, 50, func(int) string { return "192.0.2.9" }); checked != 1 {
		t.Fatalf("%d of 50 concurrent guesses were checked, want 1", checked)
	}
}

// Many addresses guessing together are held to the overall cap.
func TestGuessesFromManyClientsAreCapped(t *testing.T) {
	a, err := newAuth("hunter2", newTestStore(t))
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
		go func() { defer wg.Done(); results <- a.checkPassword(client(i), false, fmt.Sprint("guess", i)) }()
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
	h := authServer(t, "hunter2", newTestStore(t))
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

// A browser that has signed in before can sign in again while strangers keep the
// shared address locked out (everyone behind one reverse proxy) and the overall cap
// spent (many addresses at once).
func TestKnownDeviceSignsInDuringAnAttack(t *testing.T) {
	store := newTestStore(t)
	a, err := newAuth("hunter2", store)
	if err != nil {
		t.Fatal(err)
	}
	h := (&server{live: NewLive(defaultAlerts()), auth: a}).mux(newQueue(1))
	var device *http.Cookie
	for _, c := range login(t, h, "hunter2").Result().Cookies() {
		if c.Name == deviceCookie {
			device = c
		}
	}
	if device == nil {
		t.Fatal("no device cookie set on sign-in")
	}
	attempt := func(pw string, cookies ...*http.Cookie) int {
		req := httptest.NewRequest(http.MethodPost, "/login",
			strings.NewReader(url.Values{"password": {pw}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "10.0.0.2:4000" // the proxy
		for _, c := range cookies {
			req.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if code := attempt("wrong"); code != http.StatusUnauthorized {
		t.Fatalf("stranger's guess = %d, want 401", code)
	}
	guessAtOnce(a, 2*loginFailsPerSecond, func(i int) string { return fmt.Sprintf("198.51.100.%d", i) })
	if code := attempt("hunter2"); code != http.StatusTooManyRequests {
		t.Fatalf("new browser during the attack = %d, want 429", code)
	}
	forged := &http.Cookie{Name: deviceCookie, Value: "99999999999.AAAA"}
	if code := attempt("hunter2", forged); code != http.StatusTooManyRequests {
		t.Fatalf("forged device cookie = %d, want 429", code)
	}
	session := &http.Cookie{Name: deviceCookie, Value: a.token(time.Now().Add(time.Hour))}
	if code := attempt("hunter2", session); code != http.StatusTooManyRequests {
		t.Fatalf("session token as device cookie = %d, want 429", code)
	}
	if code := attempt("hunter2", device); code != http.StatusSeeOther {
		t.Fatalf("known device during the attack = %d, want 303", code)
	}
	// It is still held to one guess a second itself.
	if code := attempt("wrong", device); code != http.StatusUnauthorized {
		t.Fatalf("known device's wrong guess = %d, want 401", code)
	}
	if code := attempt("hunter2", device); code != http.StatusTooManyRequests {
		t.Fatalf("known device inside its own lockout = %d, want 429", code)
	}
}

// Another site's page must not be able to change settings through the viewer's
// browser, whether or not the UI has a password.
func TestCrossSiteWritesAreRefused(t *testing.T) {
	store := newTestStore(t)
	c := sessionFrom(t, login(t, authServer(t, "hunter2", store), "hunter2"))
	for name, h := range map[string]http.Handler{
		"signed in": authServer(t, "hunter2", store),
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
	h := authServer(t, "hunter2", newTestStore(t))
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
	store := newTestStore(t)
	h := authServer(t, "hunter2", store)
	a, _ := newAuth("hunter2", store)

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
	store := newTestStore(t)
	c := sessionFrom(t, login(t, authServer(t, "hunter2", store), "hunter2"))

	if w := get(authServer(t, "hunter2", store), "/", c); w.Code != http.StatusOK {
		t.Errorf("after restart = %d, want still signed in", w.Code)
	}
	if w := get(authServer(t, "correct horse", store), "/", c); w.Code != http.StatusSeeOther {
		t.Errorf("after password change = %d, want signed out", w.Code)
	}
}

// Startup refuses SKY_ variables it does not know, so the password must be one it does.
func TestUIPasswordIsAKnownVariable(t *testing.T) {
	if err := checkEnv(map[string]string{uiPasswordEnv: "hunter2"}); err != nil {
		t.Fatal(err)
	}
}

// postForm posts a form to h, with cookies.
func postForm(h http.Handler, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func putPassword(h http.Handler, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/password", strings.NewReader(body))
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// The first visit, before anyone has chosen, is asked to choose; once a password is
// set, it is what signs in, and it survives a restart.
func TestFirstVisitSetsAPassword(t *testing.T) {
	store := newTestStore(t)
	h := authServer(t, "", store)
	for _, path := range []string{"/", "/settings", "/login"} {
		if w := get(h, path); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/setup" && path != "/login" {
			t.Errorf("%s before setup = %d to %q, want a redirect to /setup", path, w.Code, w.Header().Get("Location"))
		}
	}
	if w := get(h, "/setup"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `name="confirm"`) {
		t.Fatalf("/setup = %d, want the form", w.Code)
	}
	if w := postForm(h, "/setup", url.Values{"password": {"hunter2"}, "confirm": {"hunter3"}}); w.Code != http.StatusBadRequest {
		t.Fatalf("mismatched confirmation = %d, want 400", w.Code)
	}
	w := postForm(h, "/setup", url.Values{"password": {"hunter2"}, "confirm": {"hunter2"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("setup = %d, want a redirect", w.Code)
	}
	c := sessionFrom(t, w)
	if w := get(h, "/", c); w.Code != http.StatusOK {
		t.Fatalf("/ with the session setup gave = %d, want 200", w.Code)
	}
	if w := get(h, "/setup"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("/setup once chosen = %d, want it gone", w.Code)
	}
	if w := postForm(h, "/setup", url.Values{"skip": {"1"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("second setup = %d", w.Code)
	}
	if w := get(h, "/"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("a second /setup must not undo the first: / = %d to %q", w.Code, w.Header().Get("Location"))
	}

	hash, _, _ := store.UIPassword()
	if strings.Contains(hash, "hunter2") || !strings.HasPrefix(hash, "pbkdf2-sha256$") {
		t.Fatalf("stored password = %q, want a hash", hash)
	}
	restarted := authServer(t, "", store)
	if w := get(restarted, "/", c); w.Code != http.StatusOK {
		t.Errorf("after restart = %d, want still signed in", w.Code)
	}
	if w := login(t, restarted, "hunter2"); w.Code != http.StatusSeeOther {
		t.Errorf("login with the stored password = %d, want 303", w.Code)
	}
}

func TestFirstVisitCanGoWithoutAPassword(t *testing.T) {
	store := newTestStore(t)
	h := authServer(t, "", store)
	if w := postForm(h, "/setup", url.Values{"skip": {"1"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("skip = %d, want a redirect", w.Code)
	}
	if w := get(h, "/"); w.Code != http.StatusOK {
		t.Fatalf("/ after skipping = %d, want the app", w.Code)
	}
	if w := get(authServer(t, "", store), "/"); w.Code != http.StatusOK {
		t.Fatalf("/ after a restart = %d, want the choice remembered", w.Code)
	}
}

// Settings changes the password, but only for someone who knows the current one, and
// the change signs every other browser out.
func TestPasswordIsChangedAndRemovedFromSettings(t *testing.T) {
	store := newTestStore(t)
	h := authServer(t, "", store)
	if w := putPassword(h, `{"password":"hunter2"}`); w.Code != http.StatusOK {
		t.Fatalf("set before anyone chose = %d: %s", w.Code, w.Body.String())
	}
	old := sessionFrom(t, login(t, h, "hunter2"))
	if w := putPassword(h, `{"current":"wrong","password":"x"}`, old); w.Code != http.StatusForbidden {
		t.Fatalf("change with the wrong current password = %d, want 403", w.Code)
	}
	time.Sleep(loginFailDelay)
	w := putPassword(h, `{"current":"hunter2","password":"correct horse"}`, old)
	if w.Code != http.StatusOK {
		t.Fatalf("change = %d: %s", w.Code, w.Body.String())
	}
	fresh := sessionFrom(t, w)
	if w := get(h, "/api/alerts", old); w.Code != http.StatusUnauthorized {
		t.Errorf("old session after a change = %d, want 401", w.Code)
	}
	if w := get(h, "/", fresh); w.Code != http.StatusOK {
		t.Errorf("the changing browser = %d, want still signed in", w.Code)
	}
	if w := putPassword(h, `{"current":"correct horse","password":""}`, fresh); w.Code != http.StatusOK {
		t.Fatalf("remove = %d: %s", w.Code, w.Body.String())
	}
	if w := get(h, "/"); w.Code != http.StatusOK {
		t.Fatalf("/ after removing = %d, want open", w.Code)
	}
}

func TestEnvPasswordIsNotEditable(t *testing.T) {
	store := newTestStore(t)
	h := authServer(t, "hunter2", store)
	c := sessionFrom(t, login(t, h, "hunter2"))
	if w := putPassword(h, `{"current":"hunter2","password":"x"}`, c); w.Code != http.StatusConflict {
		t.Fatalf("change under %s = %d, want 409", uiPasswordEnv, w.Code)
	}
	t.Setenv(uiPasswordEnv, "hunter2")
	if err := runResetPassword(io.Discard); err == nil {
		t.Fatal("-reset-password under the environment variable succeeded, want an error")
	}
}

// -reset-password, run through docker exec, clears the password from beside the running
// service, which notices and asks again as on the first visit.
func TestResetPasswordReturnsToSetup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.db")
	t.Setenv(envDBPathVar, path)
	t.Setenv(uiPasswordEnv, "")
	store, err := OpenSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	a, err := newAuth("", store)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.setPassword("forgotten"); err != nil {
		t.Fatal(err)
	}
	h := (&server{live: NewLive(defaultAlerts()), auth: a}).mux(newQueue(1))
	if err := runResetPassword(io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := a.reload(); err != nil {
		t.Fatal(err)
	}
	if w := get(h, "/"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/setup" {
		t.Fatalf("/ after a reset = %d to %q, want /setup", w.Code, w.Header().Get("Location"))
	}
}

// Two browsers signing in within the same second are still two devices: one's wrong
// guess must not lock out the other.
func TestDevicesSigningInTogetherAreThrottledApart(t *testing.T) {
	a, err := newAuth("hunter2", newTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	key := func(tok string) string {
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.AddCookie(&http.Cookie{Name: deviceCookie, Value: tok})
		k, known := a.throttleKey(req)
		if !known {
			t.Fatalf("device token %q was not recognised", tok)
		}
		return k
	}
	first, second := key(a.deviceToken(exp)), key(a.deviceToken(exp))
	if first == second {
		t.Fatal("two devices signed in at the same moment share a throttle key")
	}
	if a.checkPassword(first, true, "wrong") != loginWrong {
		t.Fatal("first device's guess was not checked")
	}
	if got := a.checkPassword(second, true, "hunter2"); got != loginOK {
		t.Fatalf("second device during the first's lockout = %v, want loginOK", got)
	}
}
