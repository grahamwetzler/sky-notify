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
	Actions  []action `json:"actions,omitempty"`
}

// ntfy renders click as an invisible tap target: the notification opens the map but
// says nothing about it. A view action is the same URL with a button the user can see.
type action struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	URL    string `json:"url"`
}

type Notifier struct {
	cfg    *Config
	client *http.Client
	url    string
	// fileURL is the same server one path deeper. ntfy takes an attachment only as the
	// raw body of a PUT to /<topic>; the JSON publish document has no room for bytes,
	// only for a URL it would have to fetch, which our :8080 is not reachable at.
	fileURL string
	// maps renders the snapshot, nil when map.enabled is false.
	maps *mapRenderer
	// shutdown is the root signal context, held purely to know when Ctrl-C has been
	// pressed. Delivery still runs on the caller's context and keeps its drain window;
	// this only takes the picture away.
	shutdown context.Context
	// history is optional: set when a store is open, nil in tests and if it failed to
	// open. Recording is never a reason for a delivery to fail.
	history *History

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
		cfg:     cfg,
		url:     base.String(),
		fileURL: base.JoinPath(cfg.Ntfy.Topic).String(),
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

	// Rendered once, before the delivery clock starts, and reused across retries: the
	// map is an attachment to this alert, not a thing to redraw each attempt.
	img := n.snapshot(ctx, a)

	// The budget must bound the whole operation, not just the sleeps between attempts:
	// three 15s HTTP attempts would otherwise block every later alert for 45s+,
	// emergencies included.
	ctx, cancel := context.WithTimeout(ctx, notifyBudget)
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		retryable, wait, err := n.send(ctx, msg, blob, img)
		// A server that will not take the attachment — a size cap, or a self-hosted ntfy
		// with its attachment cache off — rejects it permanently, and notifyLoop answers
		// a permanent rejection by backing off without a cooldown. The alert would be
		// lost for the sake of its picture. So drop the picture and say it plainly.
		if err != nil && !retryable && img != nil {
			slog.Warn("ntfy refused the map image, sending the alert without it", "icao", a.Hex, "err", err)
			img = nil
			retryable, wait, err = n.send(ctx, msg, blob, nil)
		}
		if err == nil {
			n.setErr(nil)
			n.history.Add(a.Trigger, msg, time.Now())
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

// snapshot renders the map for one alert, or returns nil when there is not going to be
// one. Every failure is silent by design: the alert is the point, the picture is not.
func (n *Notifier) snapshot(ctx context.Context, a *Alert) []byte {
	if n.maps == nil {
		return nil
	}
	shutdown := n.shutdown
	if shutdown == nil {
		shutdown = context.Background()
	}
	// Already stopping: the drain window exists to get queued alerts out, not to finish
	// drawing. Skipping outright is the difference between a late alert and no alert.
	if shutdown.Err() != nil {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, snapshotBudget)
	defer cancel()
	defer context.AfterFunc(shutdown, cancel)()
	return n.maps.snapshot(rctx, a)
}

// send makes one delivery attempt: an attachment PUT when there is an image, and
// otherwise the JSON publish this service has always used, unchanged.
func (n *Notifier) send(ctx context.Context, msg ntfyMessage, blob, img []byte) (retryable bool, wait time.Duration, err error) {
	var req *http.Request
	if img != nil {
		req, err = n.putRequest(ctx, msg, img)
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(blob))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		return false, 0, err
	}
	return n.attempt(req)
}

// putRequest carries the very same ntfyMessage render() produced, spelled as headers.
// render stays the one source of truth for what a notification says; only the transport
// differs, so the two paths cannot drift into describing an alert differently.
func (n *Notifier) putRequest(ctx context.Context, msg ntfyMessage, img []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, n.fileURL, bytes.NewReader(img))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "image/png")
	req.Header.Set("X-Filename", "map.png")
	// A header holds one line, and ntfy turns a literal backslash-n back into a newline.
	req.Header.Set("X-Message", headerSafe(strings.ReplaceAll(msg.Message, "\n", `\n`)))
	if msg.Title != "" {
		req.Header.Set("X-Title", headerSafe(msg.Title))
	}
	if msg.Priority != 0 {
		req.Header.Set("X-Priority", strconv.Itoa(msg.Priority))
	}
	if len(msg.Tags) > 0 {
		req.Header.Set("X-Tags", headerSafe(strings.Join(msg.Tags, ",")))
	}
	if msg.Click != "" {
		req.Header.Set("X-Click", headerSafe(msg.Click))
	}
	if len(msg.Actions) > 0 {
		// Quoted, because a label or a URL may contain the comma that separates fields;
		// semicolons separate one action from the next.
		parts := make([]string, 0, len(msg.Actions))
		for _, a := range msg.Actions {
			parts = append(parts, fmt.Sprintf("%s, %q, %q", a.Action, a.Label, a.URL))
		}
		req.Header.Set("X-Actions", headerSafe(strings.Join(parts, "; ")))
	}
	return req, nil
}

// headerSafe strips what cannot travel in an HTTP header. Titles and tags are built from
// plane-alert-db rows, which are third-party CSV: a stray newline in one would otherwise
// be a header injection rather than a cosmetic problem.
func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func (n *Notifier) attempt(req *http.Request) (retryable bool, wait time.Duration, err error) {
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
	if a.RuleName != "" {
		title = a.RuleName
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
	line("Callsign", strings.TrimSpace(a.AC.Flight))
	// The same fallback the registration above takes. A listed aircraft is not a typed
	// one — plane-alert-pia.csv rows carry an ICAO and little else — and asking only the
	// row would describe a listed aircraft with less than an unlisted one, dropping the
	// type code the feed did broadcast.
	acType := a.AC.Type
	if p != nil && p.Type != "" {
		acType = p.Type
	}
	if p != nil {
		line("Operator", p.Operator)
	}
	line("Type", acType)
	if p != nil {
		line("Category", p.Category)
		line("Tags", strings.Join(p.Tags, ", "))
	}
	if a.HasPass {
		when := "now"
		if a.PassIn >= time.Second {
			when = "in " + a.PassIn.Round(time.Second).String()
		}
		line("Overhead", fmt.Sprintf("closest %.1f NM %s", a.PassNM, when))
	}
	if a.Circling {
		line("Circling", "yes")
	}
	if flags := describeDBFlags(a.AC.DBFlags); flags != "" {
		line("Feeder flags", flags)
	}

	tags := []string{"airplane"}
	if a.Emergency {
		tags = append(tags, "rotating_light")
	}

	click, label := n.clickURL(a)
	var actions []action
	if click != "" {
		actions = []action{{Action: "view", Label: label, URL: click}}
	}

	return ntfyMessage{
		Topic:    n.cfg.Ntfy.Topic,
		Title:    title,
		Message:  strings.TrimRight(b.String(), "\n"),
		Priority: a.Priority,
		Tags:     tags,
		Click:    click,
		Actions:  actions,
	}
}

// clickURL returns where the notification leads and what to call the button.
func (n *Notifier) clickURL(a *Alert) (string, string) {
	if n.cfg.Tar1090URL != "" {
		if u, err := url.Parse(n.cfg.Tar1090URL); err == nil {
			q := u.Query()
			q.Set("icao", a.Hex)
			u.RawQuery = q.Encode()
			return u.String(), "Open map"
		}
	}
	// plane-alert-db links are untrusted third-party data: accept http(s) only.
	if a.Plane != nil && a.Plane.Link != "" {
		if u, err := url.Parse(a.Plane.Link); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			return u.String(), "Aircraft info"
		}
	}
	return "", ""
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
