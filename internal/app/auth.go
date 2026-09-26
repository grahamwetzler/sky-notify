package app

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
)

// auth guards the web UI and its API behind one shared password. A nil *auth is an
// open server, which is what an unset SKY_UI_PASSWORD means.
type auth struct {
	password []byte
	// key signs session cookies. It is derived from a random secret kept on disk and
	// the password together: the secret keeps sessions valid across a restart, and the
	// password in it means changing the password signs everyone out.
	key []byte
	// mu guards the throttle state, and is held across the comparison itself: checked
	// outside it, a burst of concurrent guesses would all be tested before the first
	// failure could shut the door.
	mu          sync.Mutex
	lockedUntil map[string]time.Time // by loginClient, or by device
	windowStart time.Time
	windowFails int
}

// newAuth returns nil when password is empty. secretDir holds the signing secret,
// created on first use.
func newAuth(password, secretDir string) (*auth, error) {
	if password == "" {
		return nil, nil
	}
	secret, err := sessionSecret(filepath.Join(secretDir, "session.key"))
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(password))
	return &auth{password: []byte(password), key: mac.Sum(nil), lockedUntil: map[string]time.Time{}}, nil
}

func sessionSecret(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil && len(b) >= 32 {
		return b, nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("session secret: %w", err)
	}
	b = make([]byte, 32)
	rand.Read(b)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, fmt.Errorf("session secret: %w", err)
	}
	return b, nil
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
	mac := hmac.New(sha256.New, a.key)
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
	// Hash both sides so the comparison does not leak the password's length.
	got, want := sha256.Sum256([]byte(pw)), sha256.Sum256(a.password)
	if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
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

func setCookie(w http.ResponseWriter, r *http.Request, name, path, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		// Behind a TLS-terminating proxy the request itself is plain HTTP.
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
}

// routes adds the login and logout endpoints.
func (a *auth) routes(mux *http.ServeMux) {
	page, _ := uiFS.ReadFile("login.html")
	serveLogin := func(w http.ResponseWriter, status int, msg string) {
		b := page
		if msg != "" {
			b = bytes.Replace(page, []byte("<!--error-->"),
				[]byte(`<p class="err" role="alert">`+msg+`</p>`), 1)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		w.Write(b)
	}
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if a.valid(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		serveLogin(w, http.StatusOK, "")
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		client, known := loginClient(r), false
		if d, ok := a.device(r); ok {
			client, known = "device "+d, true
		}
		switch a.checkPassword(client, known, r.PostFormValue("password")) {
		case loginWrong:
			serveLogin(w, http.StatusUnauthorized, "That password is not right.")
			return
		case loginThrottled:
			serveLogin(w, http.StatusTooManyRequests, "Too many attempts. Wait a moment and try again.")
			return
		}
		setSession(w, r, a.token(time.Now().Add(sessionTTL)), int(sessionTTL.Seconds()))
		setCookie(w, r, deviceCookie, "/login", a.deviceToken(time.Now().Add(deviceTTL)), int(deviceTTL.Seconds()))
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		setSession(w, r, "", -1)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// guard lets through only what a signed-out visitor may reach: the login page, the
// font it uses, and /healthz, which the container's own HEALTHCHECK calls with no
// cookie. The API answers 401 so the page can send itself to the login screen; a page
// load is redirected there directly.
func (a *auth) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login", "/logout", "/healthz", "/public-sans.woff2":
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
