package app

import (
	"bytes"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	uiPasswordEnv = "SKY_UI_PASSWORD"
	sessionCookie = "sky_session"
	sessionTTL    = 30 * 24 * time.Hour
	// deviceCookie marks a browser that has signed in before. It outlives the session
	// and logout, and gives that browser its own login throttle, exempt from the
	// overall cap: without it, strangers guessing through the same reverse proxy, or
	// from enough addresses at once, could keep the operator from signing back in.
	deviceCookie = "sky_device"
	deviceTTL    = 365 * 24 * time.Hour
	// After a wrong password, that client's guesses are refused unchecked for this
	// long, so it cannot guess faster than once per interval however many requests it
	// sends at once. The lockout is per client so that one stranger guessing cannot
	// keep the operator signed out.
	loginFailDelay = time.Second
	// Across all clients without a device cookie, at most this many wrong guesses are
	// checked per second: the per-client limit alone would let many addresses guess in
	// parallel.
	loginFailsPerSecond = 20
	// Past this many clients locked out at once, expired entries are swept.
	lockoutSweepAt = 1024
	// The cost of hashing a stored password. Every check of it holds the throttle lock,
	// so it is kept to tens of milliseconds on a Raspberry Pi; the throttle, not the
	// hash, is what stops guessing over the network.
	pbkdf2Iterations = 100_000
)

// authMode is where the UI's password stands.
type authMode int

const (
	// authUnset: nobody has chosen yet. A page load is sent to /setup to choose; the
	// API is open meanwhile, since there is no password to ask for.
	authUnset authMode = iota
	// authOff: the operator chose to go without a password.
	authOff
	// authStored: a password set from the UI, kept hashed in the settings database.
	authStored
	// authEnv: SKY_UI_PASSWORD, which wins over the stored one and is not editable here.
	authEnv
)

// auth guards the web UI and its API behind one shared password, which can be set,
// changed and removed while the service runs.
type auth struct {
	store  *SettingsStore
	secret []byte

	// mu guards everything below. It is held across a password check itself: checked
	// outside it, a burst of concurrent guesses would all be tested before the first
	// failure could shut the door.
	mu   sync.Mutex
	mode authMode
	// password is the plaintext for authEnv, or the stored hash for authStored.
	password string
	// key signs session cookies. It is derived from the secret and the password
	// together: the secret keeps sessions valid across a restart, and the password in it
	// means changing the password signs everyone out.
	key         []byte
	lockedUntil map[string]time.Time // by loginClient, or by device
	windowStart time.Time
	windowFails int
}

// newAuth takes the password from envPassword when it is set, or else from the store.
func newAuth(envPassword string, store *SettingsStore) (*auth, error) {
	secret, err := store.SessionSecret()
	if err != nil {
		return nil, fmt.Errorf("session secret: %w", err)
	}
	a := &auth{store: store, secret: secret, lockedUntil: map[string]time.Time{}}
	if envPassword != "" {
		a.setLocked(authEnv, envPassword)
		return a, nil
	}
	if err := a.reload(); err != nil {
		return nil, err
	}
	return a, nil
}

// reload picks up a password changed outside this process — `sky-notify
// -reset-password` run through docker exec. A no-op under SKY_UI_PASSWORD.
func (a *auth) reload() error {
	hash, chosen, err := a.store.UIPassword()
	if err != nil {
		return fmt.Errorf("ui password: %w", err)
	}
	mode := authStored
	switch {
	case !chosen:
		mode = authUnset
	case hash == "":
		mode = authOff
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mode != authEnv && (a.mode != mode || a.password != hash) {
		a.setLocked(mode, hash)
	}
	return nil
}

// watch reloads the stored password every interval until ctx is done.
func (a *auth) watch(done <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if err := a.reload(); err != nil {
				slog.Warn("could not re-read the UI password", "err", err)
			}
		}
	}
}

// setLocked switches the password. a.mu must be held, or a not yet shared.
func (a *auth) setLocked(mode authMode, password string) {
	a.mode, a.password = mode, password
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(password))
	a.key = mac.Sum(nil)
	clear(a.lockedUntil)
}

func (a *auth) state() authMode {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

// required reports whether a request needs a session.
func (a *auth) required() bool {
	m := a.state()
	return m == authStored || m == authEnv
}

// setPassword stores a new password, or none when password is empty. It refuses while
// SKY_UI_PASSWORD is set, which would win over whatever is stored here.
func (a *auth) setPassword(password string) error {
	hash := ""
	if password != "" {
		var err error
		if hash, err = hashPassword(password); err != nil {
			return err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mode == authEnv {
		return fmt.Errorf("the password is set by %s; change or remove it there", uiPasswordEnv)
	}
	if err := a.store.SetUIPassword(hash); err != nil {
		return err
	}
	mode := authOff
	if hash != "" {
		mode = authStored
	}
	a.setLocked(mode, hash)
	return nil
}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	sum, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(sum)), nil
}

// matchesLocked reports whether pw is the password. a.mu must be held.
func (a *auth) matchesLocked(pw string) bool {
	switch a.mode {
	case authEnv:
		// Hash both sides so the comparison does not leak the password's length.
		got, want := sha256.Sum256([]byte(pw)), sha256.Sum256([]byte(a.password))
		return subtle.ConstantTimeCompare(got[:], want[:]) == 1
	case authStored:
		parts := strings.Split(a.password, "$")
		if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
			return false
		}
		iter, err1 := strconv.Atoi(parts[1])
		salt, err2 := base64.RawStdEncoding.DecodeString(parts[2])
		want, err3 := base64.RawStdEncoding.DecodeString(parts[3])
		if err1 != nil || err2 != nil || err3 != nil {
			return false
		}
		got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
		return err == nil && subtle.ConstantTimeCompare(got, want) == 1
	}
	return false
}

// token is "<expiry unix>.<signature>".
func (a *auth) token(exp time.Time) string {
	e := strconv.FormatInt(exp.Unix(), 10)
	return e + "." + a.sign(e)
}

// deviceToken is a token signed under its own prefix, so a session cookie cannot be
// passed off as a device cookie or the other way round.
func (a *auth) deviceToken(exp time.Time) string {
	e := strconv.FormatInt(exp.Unix(), 10)
	return e + "." + a.sign("device "+e)
}

func (a *auth) sign(s string) string {
	a.mu.Lock()
	key := a.key
	a.mu.Unlock()
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(s))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *auth) valid(r *http.Request) bool {
	_, ok := a.cookie(r, sessionCookie, "")
	return ok
}

// device returns the signature of the request's device cookie, which identifies the
// browser for throttling, if it has a valid one.
func (a *auth) device(r *http.Request) (string, bool) {
	return a.cookie(r, deviceCookie, "device ")
}

// cookie checks the named cookie's token, signed with prefix before its expiry, and
// returns its signature.
func (a *auth) cookie(r *http.Request, name, prefix string) (string, bool) {
	c, err := r.Cookie(name)
	if err != nil {
		return "", false
	}
	e, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(a.sign(prefix+e))) {
		return "", false
	}
	exp, err := strconv.ParseInt(e, 10, 64)
	return sig, err == nil && time.Now().Unix() < exp
}

type loginResult int

const (
	loginOK loginResult = iota
	loginWrong
	// loginThrottled is a guess refused without being checked. Nothing sleeps: a
	// throttled request is answered at once, so a flood of them holds no handlers.
	loginThrottled
)

// loginClient is who a login attempt is throttled as: its source address, with IPv6
// taken as its /64, since one host commonly holds a whole /64 to rotate through.
// Behind a reverse proxy every attempt shares the proxy's address, so a stranger can
// hold off a first sign-in there; X-Forwarded-For is not trusted, as any client can
// set it. A browser that has signed in before is throttled by its device cookie instead.
func loginClient(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String()
	}
	return ip.String()
}

// throttleKey is who r's password guesses are counted against, and whether that is a
// browser that has signed in before.
func (a *auth) throttleKey(r *http.Request) (client string, known bool) {
	if d, ok := a.device(r); ok {
		return "device " + d, true
	}
	return loginClient(r), false
}

// checkPassword throttles by client. A known device, one with a valid device cookie,
// is left out of the overall cap: it has proved the password once already.
func (a *auth) checkPassword(client string, known bool, pw string) loginResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if now.Before(a.lockedUntil[client]) {
		return loginThrottled
	}
	if !known {
		if now.Sub(a.windowStart) >= time.Second {
			a.windowStart, a.windowFails = now, 0
		}
		if a.windowFails >= loginFailsPerSecond {
			return loginThrottled
		}
	}
	if a.matchesLocked(pw) {
		delete(a.lockedUntil, client)
		return loginOK
	}
	if !known {
		a.windowFails++
	}
	if len(a.lockedUntil) >= lockoutSweepAt {
		for c, until := range a.lockedUntil {
			if !now.Before(until) {
				delete(a.lockedUntil, c)
			}
		}
	}
	a.lockedUntil[client] = now.Add(loginFailDelay)
	return loginWrong
}

func setSession(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	setCookie(w, r, sessionCookie, "/", value, maxAge)
}

// signIn gives the browser a session, and marks it as one that has signed in before.
func (a *auth) signIn(w http.ResponseWriter, r *http.Request) {
	setSession(w, r, a.token(time.Now().Add(sessionTTL)), int(sessionTTL.Seconds()))
	setCookie(w, r, deviceCookie, "/", a.deviceToken(time.Now().Add(deviceTTL)), int(deviceTTL.Seconds()))
}

func setCookie(w http.ResponseWriter, r *http.Request, name, path, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		// Behind a TLS-terminating proxy the request itself is plain HTTP.
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
}

var loginPage = template.Must(template.ParseFS(uiFS, "login.html"))

// loginView is what login.html renders: the sign-in form, or with Setup the first-run
// choice of a password.
type loginView struct {
	Setup bool
	Error string
}

func serveLogin(w http.ResponseWriter, status int, v loginView) {
	var b bytes.Buffer
	if err := loginPage.Execute(&b, v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(b.Bytes())
}

// routes adds the login, logout, first-run and password endpoints.
func (a *auth) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if !a.required() || a.valid(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		serveLogin(w, http.StatusOK, loginView{})
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		if !a.required() {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		client, known := a.throttleKey(r)
		switch a.checkPassword(client, known, r.PostFormValue("password")) {
		case loginWrong:
			serveLogin(w, http.StatusUnauthorized, loginView{Error: "That password is not right."})
			return
		case loginThrottled:
			serveLogin(w, http.StatusTooManyRequests, loginView{Error: "Too many attempts. Wait a moment and try again."})
			return
		}
		a.signIn(w, r)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		setSession(w, r, "", -1)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
	// The first visit, before anyone has chosen: set a password, or go without one.
	// Once chosen this page is gone; Settings changes it from then on.
	mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		if a.state() != authUnset {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		serveLogin(w, http.StatusOK, loginView{Setup: true})
	})
	mux.HandleFunc("POST /setup", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		if a.state() != authUnset {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		pw := ""
		if r.PostFormValue("skip") == "" {
			pw = r.PostFormValue("password")
			if pw == "" {
				serveLogin(w, http.StatusBadRequest, loginView{Setup: true, Error: "Enter a password, or continue without one."})
				return
			}
			if pw != r.PostFormValue("confirm") {
				serveLogin(w, http.StatusBadRequest, loginView{Setup: true, Error: "The two passwords do not match."})
				return
			}
		}
		if err := a.setPassword(pw); err != nil {
			serveLogin(w, http.StatusInternalServerError, loginView{Setup: true, Error: err.Error()})
			return
		}
		if pw != "" {
			a.signIn(w, r)
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	// Sets, changes or removes the password from Settings. Changing an existing one
	// needs it, under the same throttle as signing in: a session left open on a shared
	// machine must not be enough to lock its owner out.
	mux.HandleFunc("PUT /api/password", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		var body struct {
			Current  string `json:"current"`
			Password string `json:"password"`
		}
		if err := decodeStrict(r.Body, &body); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		switch a.state() {
		case authEnv:
			writeJSONError(w, http.StatusConflict, fmt.Errorf("the password is set by %s; change or remove it there", uiPasswordEnv))
			return
		case authStored:
			client, known := a.throttleKey(r)
			switch a.checkPassword(client, known, body.Current) {
			case loginWrong:
				writeJSONError(w, http.StatusForbidden, errors.New("the current password is not right"))
				return
			case loginThrottled:
				writeJSONError(w, http.StatusTooManyRequests, errors.New("too many attempts; wait a moment and try again"))
				return
			}
		}
		if err := a.setPassword(body.Password); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		// The new password signs every other browser out; this one stays in.
		if body.Password != "" {
			a.signIn(w, r)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "auth": body.Password != ""})
	})
}

// guard lets through only what a signed-out visitor may reach: the login and setup
// pages, the font they use, and /healthz, which the container's own HEALTHCHECK calls
// with no cookie. The API answers 401 so the page can send itself to the login screen;
// a page load is redirected there directly. Before anyone has chosen a password, a page
// load goes to /setup to choose one.
func (a *auth) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login", "/logout", "/setup", "/healthz", "/public-sans.woff2":
			next.ServeHTTP(w, r)
			return
		}
		switch a.state() {
		case authOff:
			next.ServeHTTP(w, r)
			return
		case authUnset:
			if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/") {
				http.Redirect(w, r, "/setup", http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if a.valid(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSONError(w, http.StatusUnauthorized, errors.New("not signed in"))
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}
