package app

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
	// live is the hot-reloaded alerts, read for the AI provider at the moment an alert
	// is published. nil in tests that do not exercise research.
	live *Live
	// aiClient is the HTTP client the provider is asked with. The researcher itself is
	// built per alert, since its endpoint and model can change under us.
	aiClient *http.Client
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
		fileURL: base.JoinPath(cfg.ntfyTopic()).String(),
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
	// Both are done once, before the delivery clock starts, and reused across retries:
	// the map is an attachment to this alert and the research is a line of it, not
	// things to fetch again on each attempt.
	img := n.snapshot(ctx, a)
	a.Research = n.research(ctx, a)

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

// research asks the provider about this aircraft, or returns "" when there is not going
// to be an answer. Every failure is silent by design, exactly as the map's is: a line
// that cannot be written costs the notification that line, never the notification.
func (n *Notifier) research(ctx context.Context, a *Alert) string {
	if n.live == nil {
		return ""
	}
	shutdown := n.shutdown
	if shutdown == nil {
		shutdown = context.Background()
	}
	// Already stopping: the drain window exists to get queued alerts out, not to finish
	// asking a model who owns the aircraft.
	if shutdown.Err() != nil {
		return ""
	}
	// Asked twice, before and after the route lookup, because the answer can change while
	// we wait. The first reading is what keeps an alert that is not going to be researched
	// away from the lookup service — the route is gathered for the provider and for
	// nothing else, so no provider is as good a reason not to ask as no rule. The second
	// is the one that decides, because it is the one taken immediately before the aircraft
	// is sent to a third party.
	if !willResearch(n.live.Get(), a) {
		return ""
	}
	// On ctx and its own budget, never the provider's: an unreachable lookup service must
	// cost the model a line of context, not the answer itself. Publish has already spent
	// up to the map budget by now, so what is left is the notify budget, which this shares
	// with the call below rather than borrowing from it.
	a.Route = n.route(ctx, a)

	cfg := n.live.Get()
	if !willResearch(cfg, a) {
		return ""
	}
	prompt := researchPromptFor(cfg, a.Trigger)
	rs := newResearcher(cfg, n.aiClient)
	rctx, cancel := context.WithTimeout(ctx, cfg.AI.Timeout.Std())
	defer cancel()
	defer context.AfterFunc(shutdown, cancel)()
	answer, err := rs.ask(rctx, prompt, a.facts())
	if err != nil {
		slog.Warn("research failed, sending the alert without it", "icao", a.Hex, "err", err)
		return ""
	}
	return answer
}

// willResearch reports whether this alert is going to be put to a provider under these
// rules: a rule that asks, and a provider to ask. Both are hot-reloaded, so it is asked
// again after anything that waits.
func willResearch(cfg *Alerts, a *Alert) bool {
	// Publish is past the queue's last gate and will deliver whatever it has prepared —
	// an alert already in flight is not recalled — but a rule muted or excepted while the
	// map was drawing has still said not to alert on this aircraft, and that is reason
	// enough not to hand it to a third party and be billed for the answer.
	if claimedBy(cfg, a) == nil {
		return false
	}
	if researchPromptFor(cfg, a.Trigger) == "" {
		return false
	}
	if !cfg.aiEnabled() {
		slog.Warn("a rule asked for research but no AI provider is configured", "icao", a.Hex)
		return false
	}
	return true
}

// route asks the lookup service where this callsign flies between, for the facts block and
// nothing else. Called from research and only from research: the service is a third party,
// and an alert nobody is asking a model about has no reason to reach one. A failure costs
// the model one line of context, never the notification.
func (n *Notifier) route(ctx context.Context, a *Alert) string {
	callsign := strings.TrimSpace(a.AC.Flight)
	if n.cfg.RouteAPIURL == "" || callsign == "" || a.AC.Lat == nil || a.AC.Lon == nil {
		return ""
	}
	rctx, cancel := context.WithTimeout(ctx, routeBudget)
	defer cancel()
	if shutdown := n.shutdown; shutdown != nil {
		defer context.AfterFunc(shutdown, cancel)()
	}
	r, err := lookupRoute(rctx, n.aiClient, n.cfg.RouteAPIURL, callsign, *a.AC.Lat, *a.AC.Lon)
	if err != nil {
		slog.Warn("route lookup failed, researching without it", "icao", a.Hex, "err", err)
		return ""
	}
	return r
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
	case n.cfg.ntfyToken() != "":
		req.Header.Set("Authorization", "Bearer "+n.cfg.ntfyToken())
	case n.cfg.Ntfy.User != "":
		req.SetBasicAuth(n.cfg.Ntfy.User, n.cfg.ntfyPassword())
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
	line := func(k, v string) { writeLine(&b, k, v) }
	// A research answer replaces the notification body outright: it is the one sentence
	// someone reads on a phone before the shade collapses, and the Key: value lines below
	// are the facts the model was already given to write it, not more for the reader.
	switch {
	case a.Research != "":
		b.WriteString(researchMark + a.Research)
	default:
		if a.Emergency {
			line("Emergency", a.Squawk+" ("+a.SquawkMeans+")")
		}
		line("Registration", a.reg())
		line("Callsign", strings.TrimSpace(a.AC.Flight))
		if p != nil {
			line("Operator", p.Operator)
		}
		line("Type", a.acType())
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
		// What actually happened, next to the Overhead line above, which is the prediction a
		// passes_within_nm condition recorded. A rule may state both.
		if a.AtClosest && a.HasDistance {
			line("Closest pass", fmt.Sprintf("%.1f NM", a.DistanceNM))
		}
		if a.Circling {
			line("Circling", "yes")
		}
		if flags := describeDBFlags(a.AC.DBFlags); flags != "" {
			line("Feeder flags", flags)
		}
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
		Topic:    n.cfg.ntfyTopic(),
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

// writeLine is the "Key: value" line every notification body and every facts block is
// built from. Shared so the two cannot drift into spelling a field differently.
func writeLine(b *strings.Builder, k, v string) {
	if v != "" {
		fmt.Fprintf(b, "%s: %s\n", k, v)
	}
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
