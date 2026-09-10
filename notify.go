package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const notifyBudget = 30 * time.Second

type ntfyMessage struct {
	Topic    string   `json:"topic"`
	Message  string   `json:"message"`
	Title    string   `json:"title,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Click    string   `json:"click,omitempty"`
}

type Notifier struct {
	cfg    *Config
	client *http.Client
	url    string

	mu      sync.Mutex
	lastErr error
}

func NewNotifier(cfg *Config) (*Notifier, error) {
	base, err := url.Parse(cfg.Ntfy.URL)
	if err != nil {
		return nil, err
	}
	// Publish to the server root, NOT /<topic>: ntfy only parses the body as a publish
	// document at the root. POSTing this JSON to /<topic> "succeeds" with a 200 and
	// delivers the raw JSON text as the message body. The topic travels in the payload.
	return &Notifier{
		cfg: cfg,
		url: base.String(),
		client: &http.Client{
			Timeout: 15 * time.Second,
			// Go's default would silently downgrade a redirected POST to a GET and hand
			// back a 200, writing the cooldown for something never published.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (n *Notifier) LastErr() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lastErr
}

func (n *Notifier) setErr(err error) {
	n.mu.Lock()
	n.lastErr = err
	n.mu.Unlock()
}

// Publish sends one alert, retrying transient failures within a bounded budget. Any
// exhausted delivery — retryable or not — is recorded as the last error, because a
// wedged resolver or a sustained 429 starves alerts exactly as completely as a bad token.
func (n *Notifier) Publish(ctx context.Context, a *Alert) error {
	msg := n.render(a)
	blob, err := json.Marshal(msg)
	if err != nil {
		n.setErr(err)
		return err
	}

	// The budget must bound the whole operation, not just the sleeps between attempts:
	// three 15s HTTP attempts would otherwise block every later alert for 45s+,
	// emergencies included.
	ctx, cancel := context.WithTimeout(ctx, notifyBudget)
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		retryable, wait, err := n.attempt(ctx, blob)
		if err == nil {
			n.setErr(nil)
			return nil
		}
		lastErr = err
		if !retryable {
			slog.Error("notification rejected, not retrying", "err", err)
			break
		}
		if attempt == 3 {
			break
		}
		backoff := time.Duration(attempt) * time.Second
		if wait > backoff {
			backoff = wait
		}
		// Cap any server-supplied Retry-After to the remaining budget: an arbitrary
		// delay must not stall the notifier for an hour.
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			if backoff > remaining {
				backoff = remaining
			}
		}
		select {
		case <-ctx.Done():
			n.setErr(ctx.Err())
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	n.setErr(lastErr)
	return lastErr
}

func (n *Notifier) attempt(ctx context.Context, blob []byte) (retryable bool, wait time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(blob))
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	switch {
	case n.cfg.Ntfy.Token != "":
		req.Header.Set("Authorization", "Bearer "+n.cfg.Ntfy.Token)
	case n.cfg.Ntfy.User != "":
		req.SetBasicAuth(n.cfg.Ntfy.User, n.cfg.Ntfy.Password)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return true, 0, err // network-level: transient
	}
	defer resp.Body.Close()
	// Body is drained but discarded; only the status matters.
	readLimited(resp.Body, 1<<20)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, 0, nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// Redirects are disabled, so this means the endpoint moved: a config problem.
		return false, 0, fmt.Errorf("ntfy redirected (%s) to %q; point ntfy.url at the final address",
			resp.Status, resp.Header.Get("Location"))
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return true, parseRetryAfter(resp.Header.Get("Retry-After")), fmt.Errorf("ntfy returned %s", resp.Status)
	default:
		return false, 0, fmt.Errorf("ntfy returned %s", resp.Status)
	}
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func (n *Notifier) render(a *Alert) ntfyMessage {
	p := a.Plane
	title := a.Hex
	switch {
	case p != nil && p.Operator != "" && p.Type != "":
		title = p.Operator + " " + p.Type
	case p != nil && p.Operator != "":
		title = p.Operator
	case p != nil && p.Reg != "":
		title = p.Reg
	case a.AC.Reg != "":
		title = a.AC.Reg
	}
	if a.Emergency {
		title = "SQUAWK " + a.Squawk + " — " + title
	}

	var b strings.Builder
	line := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	if a.Emergency {
		line("Emergency", a.Squawk+" ("+a.SquawkMeans+")")
	}
	reg := a.AC.Reg
	if p != nil && p.Reg != "" {
		reg = p.Reg
	}
	line("Registration", reg)
	line("ICAO", a.Hex)
	line("Callsign", strings.TrimSpace(a.AC.Flight))
	if p != nil {
		line("Operator", p.Operator)
		line("Type", p.Type)
		line("Category", p.Category)
		line("Tags", strings.Join(p.Tags, ", "))
	} else if a.AC.Type != "" {
		line("Type", a.AC.Type)
	}
	switch {
	case a.AC.AltBaro.Ground:
		line("Altitude", "on ground")
	case a.AC.AltBaro.Present:
		line("Altitude", fmt.Sprintf("%d ft", a.AC.AltBaro.Feet))
	}
	if a.AC.GS > 0 {
		line("Ground speed", fmt.Sprintf("%.0f kt", a.AC.GS))
	}
	if a.HasDistance {
		line("Distance", fmt.Sprintf("%.1f NM", a.DistanceNM))
	}
	if flags := describeDBFlags(a.AC.DBFlags); flags != "" {
		line("Feeder flags", flags)
	}

	tags := []string{"airplane"}
	priority := n.cfg.Ntfy.Priority
	if a.Emergency {
		tags = append(tags, "rotating_light")
		priority = 5
	}

	return ntfyMessage{
		Topic:    n.cfg.Ntfy.Topic,
		Title:    title,
		Message:  strings.TrimRight(b.String(), "\n"),
		Priority: priority,
		Tags:     tags,
		Click:    n.clickURL(a),
	}
}

func (n *Notifier) clickURL(a *Alert) string {
	if n.cfg.Tar1090URL != "" {
		if u, err := url.Parse(n.cfg.Tar1090URL); err == nil {
			q := u.Query()
			q.Set("icao", a.Hex)
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	// plane-alert-db links are untrusted third-party data: accept http(s) only.
	if a.Plane != nil && a.Plane.Link != "" {
		if u, err := url.Parse(a.Plane.Link); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			return u.String()
		}
	}
	return ""
}

func describeDBFlags(f int) string {
	var out []string
	for _, b := range []struct {
		bit  int
		name string
	}{{1, "military"}, {2, "interesting"}, {4, "PIA"}, {8, "LADD"}} {
		if f&b.bit != 0 {
			out = append(out, b.name)
		}
	}
	return strings.Join(out, ", ")
}
