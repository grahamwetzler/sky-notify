package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

func (s *server) mux(q *queue) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		b, _ := uiFS.ReadFile("ui.html")
		w.Write(b)
	})
	// The page's one asset. The URL is fixed, so it cannot be cached immutably: a
	// binary that ships a different font would leave every browser that had already
	// fetched this one rendering the old one until its entry expired. The ETag is the
	// font's own content, so a repeat visit costs a 304 and a changed font is picked
	// up on the next request.
	font, _ := uiFS.ReadFile("public-sans.woff2")
	sum := sha256.Sum256(font)
	fontETag := fmt.Sprintf("%q", hex.EncodeToString(sum[:8]))
	mux.HandleFunc("GET /public-sans.woff2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "font/woff2")
		w.Header().Set("ETag", fontETag)
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "public-sans.woff2", time.Time{}, bytes.NewReader(font))
	})
	mux.HandleFunc("GET /api/alerts", func(w http.ResponseWriter, r *http.Request) {
		env := environMap(os.Environ())
		alerts, err := loadAlertsFile(s.store)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		// Each locked setting carries its value as well as its name. The page warns
		// about rules this server would reject, and the save validates the file with
		// the environment laid over it — so without the value here, a distance rule on
		// a receiver whose coordinates arrive as SKY_LAT and SKY_LON would be marked
		// invalid against a file that never mentions them.
		var locked []map[string]string
		for _, b := range alertEnvBindings() {
			v, ok := env[b.name]
			if !ok {
				continue
			}
			l := map[string]string{"key": b.key, "env": b.name, "value": v}
			// Except a credential. The page only has to know the key is not its to
			// edit; the value is the one thing in this response that must not be.
			//
			// An empty value is not a credential, so it is safe to show — and the page
			// needs to see it: applyAlertEnv reapplies SKY_AI_KEY on every reload, so a
			// key typed here would be saved and then silently discarded, not used. The
			// row stays locked so that trap stays visible, but an empty value must not
			// pin ai.url the way a real key does — refuseAIKeyMove already lets the
			// endpoint move when the variable holds no credential, so a keyless
			// deployment can still change its provider from here.
			if b.key == aiKeyPath && v != "" {
				delete(l, "value")
			}
			locked = append(locked, l)
		}
		// The cooldown key of every rule, in order. A rule may have no name, and the
		// page has to be able to say what the ledger and the logs will call it — which
		// only this side can compute, so it is not left to the page to guess.
		keys := make([]string, len(alerts.Rules))
		for i := range alerts.Rules {
			keys[i] = alerts.Rules[i].Key()
		}
		// The API key is handled the way a password field is: never echoed back, only
		// reported as set or not. alerts is a fresh document per request, so blanking it
		// here changes nothing on disk — and a PUT that sends no key keeps the stored one.
		keySet := alerts.aiKey() != ""
		alerts.AI.Key = nil
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"alerts": alerts, "locked": locked,
			"rule_keys": keys, "vocabulary": s.db.Vocabulary(), "feed_types": s.feedTypes(),
			"ai_key_set": keySet, "research_prompt_default": defaultResearchPrompt,
		})
	})
	mux.HandleFunc("POST /api/preview", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var rule Rule
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rule); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		// A negative or unparseable offset reads as the first page rather than as an
		// error: it can only come from our own page, and showing the top of the list is
		// a better answer than refusing to preview at all.
		offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
		if err != nil || offset < 0 {
			offset = 0
		}
		total, sample := s.db.Match(rule, previewLimit, offset)
		rows, _ := s.db.Stats()
		body := map[string]any{
			"total": total, "aircraft": sample, "database": rows,
			"limit": previewLimit, "offset": offset, "key": rule.Key(),
		}
		// What the rule would alert on this second, judged against the last poll rather
		// than the database: the whole rule applies here, altitude and distance and
		// flight path included. Not paged — the sky holds a few hundred aircraft, not a
		// few hundred thousand, so the sample cap is only ever a courtesy.
		overhead, circling := s.snapshot()
		liveTotal, liveSample := MatchLive(rule, overhead, circling, s.db, s.previewAlerts(r.URL.Query()), previewLimit)
		age, seen, fresh := s.pollAge()
		live := map[string]any{
			"total": liveTotal, "aircraft": liveSample, "overhead": len(overhead),
			"fresh": fresh,
		}
		if seen {
			live["age_s"] = int(age.Seconds())
		}
		body["live"] = live
		// Faceting scans the whole database once per pickable field, and the facets do
		// not change as you page through a fixed rule. Only the first page pays for it.
		if offset == 0 {
			body["facets"] = s.db.Facets(rule)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		// The rule's cooldown key, which is what deliveries were filed under. Blank is
		// an empty page, not an error: a rule the page has not saved yet has no history.
		// `after` is the cursor a previous page handed back, absent on the first.
		q := r.URL.Query()
		page, err := s.history.List(q.Get("key"), q.Get("after"), historyPage)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("PUT /api/alerts", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		alerts := defaultAlerts()
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(alerts); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		// Validate what the service will actually run: the file as written, with the
		// environment laid over it exactly as LoadAlerts does. The overlay is a copy, so
		// the file keeps the operator's own values — an override is not silently baked in.
		// Only the env bindings' scalar fields are reassigned, and none of them alias the
		// rules slice, so a shallow copy is enough to keep the payload untouched.
		env := environMap(os.Environ())
		// The stored document, needed twice below. A row that cannot be read is a
		// refusal rather than a silent fresh start: the save is about to overwrite it,
		// and defaulting would write away the credential it holds without saying so.
		// Nothing is lost by refusing — GET answers 500 on the same row, so a page in
		// this state could not have loaded either.
		onDisk, err := loadAlertsFile(s.store)
		if err != nil {
			writeJSONError(w, http.StatusConflict, fmt.Errorf("alerts cannot be read, so saving over it would discard what it holds: %w", err))
			return
		}
		// A key the page was never shown must not be redirected by it. Carrying the
		// stored credential to an endpoint this save named would post the secret to
		// whatever host the payload chose — the same disclosure as serving it outright,
		// by a longer route — and it is the accidental case too: switching providers
		// would hand the new one the old one's key.
		if err := checkAIKeyStaysPut(onDisk, alerts, env); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		// No key in the payload means "leave the stored one alone": the page is never
		// sent it, so it has nothing to send back, and a save that only changed the poll
		// interval must not wipe the credential. An empty string is the page saying
		// clear it, which is a different thing and is honoured.
		//
		// Turning research off takes the key with it. A credential kept behind an empty
		// endpoint has no origin left for the guard above to judge the next save's move
		// against, so keeping it would make off-then-on a way to point it anywhere —
		// and would refuse the honest re-enable with an error naming no origin at all.
		// Off for the run is not off: a variable that blanks the endpoint leaves the
		// file's own, and the key it was issued for, alone.
		switch {
		case aiKeyEndpoint(alerts, env) == "":
			alerts.AI.Key = nil
		case alerts.AI.Key == nil:
			alerts.AI.Key = onDisk.AI.Key
		case *alerts.AI.Key == "":
			alerts.AI.Key = nil
		}
		overlaid := *alerts
		if err := applyAlertEnv(&overlaid, env); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if err := overlaid.validate(); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		blob, err := json.Marshal(alerts)
		var version int64
		if err == nil {
			version, err = s.store.Put("alerts", string(blob))
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": version})
	})
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		env := environMap(os.Environ())
		cfg, err := loadConfigFile(s.store)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		var locked []map[string]string
		for _, b := range envBindings() {
			v, ok := env[b.name]
			if !ok {
				continue
			}
			l := map[string]string{"key": b.key, "env": b.name, "value": v}
			// Secrets are reported locked without their value, the same treatment
			// ai.key gets. An empty value is not a credential, so it stays visible.
			if (b.key == "ntfy.token" || b.key == "ntfy.password") && v != "" {
				delete(l, "value")
			}
			locked = append(locked, l)
		}
		tokenSet, passwordSet := cfg.ntfyToken() != "", cfg.ntfyPassword() != ""
		cfg.Ntfy.Token, cfg.Ntfy.Password = nil, nil
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"config": cfg, "locked": locked,
			"ntfy_token_set": tokenSet, "ntfy_password_set": passwordSet,
		})
	})
	mux.HandleFunc("PUT /api/config", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		cfg := defaultConfig()
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(cfg); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		env := environMap(os.Environ())
		// The stored document, needed twice below, the same reason PUT /api/alerts reads
		// it: a row that cannot be read is a refusal rather than a silent fresh start.
		onDisk, err := loadConfigFile(s.store)
		if err != nil {
			writeJSONError(w, http.StatusConflict, fmt.Errorf("config cannot be read, so saving over it would discard what it holds: %w", err))
			return
		}
		// A token or password the page was never shown must not be redirected by it — the
		// config-side mirror of the ai.key guard on PUT /api/alerts.
		if err := checkNtfyCredsStayPut(onDisk, cfg, env); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		// No credential in the payload means "leave the stored one alone" — the page is
		// never sent it, so PUT /api/config's own zero value here must not read as a
		// clear. An empty string is the page saying clear it, which is honoured.
		if cfg.Ntfy.Token == nil {
			cfg.Ntfy.Token = onDisk.Ntfy.Token
		} else if *cfg.Ntfy.Token == "" {
			cfg.Ntfy.Token = nil
		}
		if cfg.Ntfy.Password == nil {
			cfg.Ntfy.Password = onDisk.Ntfy.Password
		} else if *cfg.Ntfy.Password == "" {
			cfg.Ntfy.Password = nil
		}
		overlaid := *cfg
		if err := applyConfigEnv(&overlaid, env); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if err := overlaid.validate(); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		blob, err := json.Marshal(cfg)
		var version int64
		if err == nil {
			version, err = s.store.Put("config", string(blob))
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// The live process objects (httpClient, DB, tile store, notifier) are built
		// once at startup from the config LoadConfig read then; saving here has no
		// effect until the process restarts, same as the config file always required.
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": version, "restart_required": true})
	})
	mux.HandleFunc("GET /api/config/export", func(w http.ResponseWriter, r *http.Request) {
		redact := r.URL.Query().Get("redact") == "true"
		doc, err := exportSettings(s.store, redact)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("POST /api/config/import", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var doc importDocument
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&doc); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		env := environMap(os.Environ())
		if err := importSettings(s.store, &doc, env); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		// The alerts document takes effect immediately rather than waiting for the
		// next poll of the watcher, the same as any other write to that row.
		if len(doc.Alerts) > 0 {
			if reloaded, err := LoadAlerts(os.Environ(), s.store); err == nil {
				s.live.p.Store(reloaded)
			}
		}
		resp := map[string]any{"ok": true}
		if len(doc.Config) > 0 {
			resp["restart_required"] = true
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		lastPoll, pollErr, aircraft := s.lastFreshPoll, s.lastPollErr, s.aircraft
		s.mu.Unlock()

		rows, refreshed := s.db.Stats()
		notifyErr := s.notifier.LastErr()
		stateErr := s.state.WriteErr()

		var reasons []string
		stale := s.live.Get().Source.PollInterval.Std() * 3
		if lastPoll.IsZero() {
			reasons = append(reasons, "no successful poll yet")
		} else if age := time.Since(lastPoll); age > stale {
			reasons = append(reasons, fmt.Sprintf("last fresh poll %s ago", age.Round(time.Second)))
		}
		// A service that cannot deliver, or cannot record what it delivered, has
		// stopped doing its job — green health there would hide it for weeks.
		if notifyErr != nil {
			reasons = append(reasons, "notification sink failing: "+notifyErr.Error())
		}
		if stateErr != nil {
			reasons = append(reasons, "state write failing: "+stateErr.Error())
		}

		body := map[string]any{
			"status":   "ok",
			"aircraft": aircraft,
			"db_rows":  rows,
			"pending":  q.depth(),
		}
		if !lastPoll.IsZero() {
			body["last_fresh_poll_age_s"] = int(time.Since(lastPoll).Seconds())
		}
		if !refreshed.IsZero() {
			body["db_refresh_age_s"] = int(time.Since(refreshed).Seconds())
		}
		if pollErr != nil {
			body["last_poll_error"] = pollErr.Error()
		}
		if notifyErr != nil {
			body["last_notify_error"] = notifyErr.Error()
		}

		status := http.StatusOK
		if len(reasons) > 0 {
			status, body["status"], body["reasons"] = http.StatusServiceUnavailable, "unhealthy", reasons
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	})
	return mux
}

func writeJSONError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
