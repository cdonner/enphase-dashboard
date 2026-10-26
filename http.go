package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSONResponse(w, status, map[string]string{"error": err.Error()})
}

// errStatus maps upstream auth failures to 401, everything else to `fallback`.
func errStatus(err error, fallback int) int {
	var hs *httpStatusError
	if errors.Is(err, errUnauthorized) || (errors.As(err, &hs) && hs.status == 401) {
		return 401
	}
	return fallback
}

func (s *Server) routes(dist fs.FS) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var lastSample *int64
		if s.live != nil {
			lastSample = ptr(s.live.At)
		}
		writeJSONResponse(w, 200, map[string]any{
			"gatewayHost":  s.cfg.EnvoyHost,
			"gateway":      s.gwInfo,
			"token":        tokenInfo(s.token),
			"autoRenew":    s.credentials != nil,
			"renewError":   s.renewError,
			"lastSampleAt": lastSample,
			"lastError":    s.lastError,
			"pollMs":       s.cfg.Poll.Milliseconds(),
			"cloud":        s.cloud.status(),
			"mqtt":         s.mqtt.status(),
		})
	})

	// Paste the code shown after approving access at cloud.authorizeUrl.
	mux.HandleFunc("POST /api/cloud/connect", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Code string `json:"code"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if err := s.cloud.connect(body.Code); err != nil {
			writeErr(w, 400, err)
			return
		}
		s.mu.Lock()
		s.cloudMeta = CloudMeta{}
		s.mu.Unlock()
		go s.cloudTick()
		writeJSONResponse(w, 200, map[string]bool{"ok": true})
	})

	// Sign in to Enlighten and request a token for this gateway. Credentials are
	// only kept (data/credentials.json, mode 600) when `remember` is set.
	mux.HandleFunc("POST /api/token/create", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email    string `json:"email"`
			Password string `json:"password"`
			Remember bool   `json:"remember"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Email == "" || body.Password == "" {
			writeErr(w, 400, errors.New("Email and password are required"))
			return
		}
		token, err := s.requestToken(body.Email, body.Password)
		if err == nil {
			err = s.verifyAndSaveToken(token)
		}
		if err != nil {
			writeErr(w, errStatus(err, 502), err)
			return
		}
		if body.Remember {
			creds := &Credentials{Email: body.Email, Password: body.Password}
			s.mu.Lock()
			s.credentials = creds
			s.mu.Unlock()
			if err := s.writeJSON("credentials.json", creds, 0o600); err != nil {
				writeErr(w, 500, err)
				return
			}
		}
		writeJSONResponse(w, 200, map[string]any{"ok": true, "token": tokenInfo(token)})
	})

	// Save a token generated elsewhere (Entrez web UI, entrez-auth-token URL).
	mux.HandleFunc("POST /api/token", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if err := s.verifyAndSaveToken(body.Token); err != nil {
			writeErr(w, errStatus(err, 400), err)
			return
		}
		writeJSONResponse(w, 200, map[string]any{"ok": true, "token": tokenInfo(s.currentToken())})
	})

	mux.HandleFunc("DELETE /api/token", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.token, s.credentials, s.live = "", nil, nil
		s.mu.Unlock()
		s.removeFile("token.json")
		s.removeFile("credentials.json")
		writeJSONResponse(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("DELETE /api/credentials", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.credentials = nil
		s.mu.Unlock()
		s.removeFile("credentials.json")
		writeJSONResponse(w, 200, map[string]bool{"ok": true})
	})

	// Endpoints below need a gateway token.
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if s.currentToken() == "" {
				writeErr(w, 401, errors.New("No gateway token"))
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("GET /api/live", authed(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var sevenDays *float64
		if s.pdm != nil && s.pdm.Production.Pcu != nil {
			sevenDays = ptr(s.pdm.Production.Pcu.WattHoursSevenDays)
		}
		writeJSONResponse(w, 200, map[string]any{
			"live":              s.live,
			"today":             s.publicDaily(dayKey(time.Now())),
			"producedSevenDays": sevenDays,
		})
	}))

	mux.HandleFunc("GET /api/today", authed(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.rollDay(dayKey(time.Now()))
		writeJSONResponse(w, 200, s.today)
	}))

	mux.HandleFunc("GET /api/daily", authed(func(w http.ResponseWriter, r *http.Request) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		if days <= 0 {
			days = 14
		}
		days = min(days, 90)
		s.mu.Lock()
		defer s.mu.Unlock()
		out := make([]PublicDay, days)
		t := time.Now()
		for i := days - 1; i >= 0; i-- {
			out[i] = s.publicDaily(dayKey(t))
			t = t.AddDate(0, 0, -1)
		}
		writeJSONResponse(w, 200, out)
	}))

	mux.HandleFunc("GET /api/ev", authed(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		type chargerView struct {
			EvCharger
			Sessions []EvSessionView `json:"sessions"`
		}
		rates := s.currentRates()
		chargers := []chargerView{}
		for _, c := range s.ev.Chargers {
			cv := chargerView{EvCharger: c, Sessions: []EvSessionView{}}
			for _, sess := range c.Sessions {
				cv.Sessions = append(cv.Sessions, s.sessionView(c.Serial, sess, rates))
			}
			chargers = append(chargers, cv)
		}
		writeJSONResponse(w, 200, map[string]any{"at": s.ev.At, "chargers": chargers, "settings": s.settings, "rates": rates.info})
	}))

	// Appliance activity for a day (default today), from the 1-second event detector.
	mux.HandleFunc("GET /api/appliances/day", authed(func(w http.ResponseWriter, r *http.Request) {
		date := r.URL.Query().Get("date")
		if date == "" {
			date = dayKey(time.Now())
		}
		if _, err := time.Parse("2006-01-02", date); err != nil {
			writeErr(w, 400, errors.New("date must be YYYY-MM-DD"))
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		var open []Run
		if date == dayKey(time.Now()) {
			open = s.detector.open
		}
		runs := s.loadDayRuns(date)
		legs := len(s.detector.legs)
		if legs == 0 && len(runs) > 0 {
			legs = len(runs[0].DP)
		}
		writeJSONResponse(w, 200, map[string]any{
			"date":       date,
			"legs":       legs,
			"runs":       len(runs),
			"activities": s.activities(runs, open, nowMs()),
		})
	}))

	// Unnamed signatures of the last week, for naming in batches, plus the named ones.
	mux.HandleFunc("GET /api/appliances", authed(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		type named struct {
			Appliance
			Described []Signature `json:"described"`
		}
		list := []named{}
		for _, a := range s.appliances {
			n := named{Appliance: a, Described: []Signature{}}
			for _, sig := range a.Signatures {
				n.Described = append(n.Described, describe(sig.DP, sig.DQ))
			}
			list = append(list, n)
		}
		clusters, groups := s.clustersAndGroups()
		writeJSONResponse(w, 200, map[string]any{
			"appliances": list, "unnamed": clusters, "groups": groups, "aiProviders": aiProviders(),
			"household": s.settings.Household, "inventory": s.settings.Inventory, "catalog": catalogEntries(),
		})
	}))

	// Name a signature. A name that already exists adds the signature to that appliance.
	mux.HandleFunc("POST /api/appliances", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name   string    `json:"name"`
			DP     []float64 `json:"dp"`
			DQ     []float64 `json:"dq"`
			Ignore bool      `json:"ignore"` // hide this signature instead of naming it
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (strings.TrimSpace(body.Name) == "" && !body.Ignore) || len(body.DP) == 0 || len(body.DQ) != len(body.DP) {
			writeErr(w, 400, errors.New("name (or ignore), dp and dq are required"))
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		var a Appliance
		if body.Ignore {
			a = s.ignoreSignature(Sig{DP: body.DP, DQ: body.DQ})
		} else {
			a = s.addSignature(strings.TrimSpace(body.Name), Sig{DP: body.DP, DQ: body.DQ})
		}
		if err := s.saveAppliances(); err != nil {
			writeErr(w, 500, err)
			return
		}
		s.mqtt.appliancesChanged()
		writeJSONResponse(w, 200, a)
	})

	// Household description used as context for the AI suggestions.
	mux.HandleFunc("PUT /api/household", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Household string `json:"household"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Household) > 4000 {
			writeErr(w, 400, errors.New("household text (up to 4000 characters) is required"))
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		st := s.settings
		st.Household = strings.TrimSpace(body.Household)
		if err := s.writeJSON("settings.json", st, 0o644); err != nil {
			writeErr(w, 500, err)
			return
		}
		s.settings = st
		writeJSONResponse(w, 200, map[string]bool{"ok": true})
	})

	// The owner's appliance list: catalog marks and custom appliances.
	mux.HandleFunc("PUT /api/inventory", func(w http.ResponseWriter, r *http.Request) {
		var inv Inventory
		if err := json.NewDecoder(r.Body).Decode(&inv); err != nil {
			writeErr(w, 400, err)
			return
		}
		for i := range inv.Custom {
			inv.Custom[i].Name = strings.TrimSpace(inv.Custom[i].Name)
		}
		if err := inv.validate(); err != nil {
			writeErr(w, 400, err)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		st := s.settings
		st.Inventory = &inv
		if err := s.writeJSON("settings.json", st, 0o644); err != nil {
			writeErr(w, 500, err)
			return
		}
		s.settings = st
		s.clusterCache.at = 0 // suggestions depend on the inventory
		writeJSONResponse(w, 200, inv)
	})

	// The same request as text, for pasting into a chat app instead of using the API.
	mux.HandleFunc("GET /api/appliances/ai-prompt", authed(func(w http.ResponseWriter, r *http.Request) {
		text, err := s.chatPrompt()
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSONResponse(w, 200, map[string]string{"prompt": text})
	}))

	mux.HandleFunc("POST /api/appliances/ask-ai", authed(func(w http.ResponseWriter, r *http.Request) {
		res, err := s.askAI(r.Context(), r.URL.Query().Get("provider"))
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		writeJSONResponse(w, 200, res)
	}))

	mux.HandleFunc("PUT /api/appliances/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || strings.TrimSpace(body.Name) == "" {
			writeErr(w, 400, errors.New("name is required"))
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for i := range s.appliances {
			if s.appliances[i].ID == r.PathValue("id") {
				s.appliances[i].Name = strings.TrimSpace(body.Name)
				if err := s.saveAppliances(); err != nil {
					writeErr(w, 500, err)
					return
				}
				s.mqtt.appliancesChanged()
				writeJSONResponse(w, 200, s.appliances[i])
				return
			}
		}
		writeErr(w, 404, errors.New("no such appliance"))
	})

	// Label one signature of an appliance (e.g. "warm-up").
	mux.HandleFunc("PUT /api/appliances/{id}/signatures/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.PathValue("n"))
		var body struct {
			Label string `json:"label"`
		}
		if err != nil || json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Label) > 80 {
			writeErr(w, 400, errors.New("label (up to 80 characters) is required"))
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for i := range s.appliances {
			a := &s.appliances[i]
			if a.ID == r.PathValue("id") && n >= 0 && n < len(a.Signatures) {
				a.Signatures[n].Label = strings.TrimSpace(body.Label)
				if err := s.saveAppliances(); err != nil {
					writeErr(w, 500, err)
					return
				}
				writeJSONResponse(w, 200, *a)
				return
			}
		}
		writeErr(w, 404, errors.New("no such appliance or signature"))
	})

	// Move one signature to another (existing or new) appliance.
	mux.HandleFunc("POST /api/appliances/{id}/signatures/{n}/move", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.PathValue("n"))
		var body struct {
			Name string `json:"name"`
		}
		if err != nil || json.NewDecoder(r.Body).Decode(&body) != nil || strings.TrimSpace(body.Name) == "" {
			writeErr(w, 400, errors.New("name is required"))
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		a, err := s.moveSignature(r.PathValue("id"), n, strings.TrimSpace(body.Name))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		if err := s.saveAppliances(); err != nil {
			writeErr(w, 500, err)
			return
		}
		s.mqtt.appliancesChanged()
		writeJSONResponse(w, 200, a)
	})

	// Remove one signature from an appliance (the appliance goes when its last one does).
	mux.HandleFunc("DELETE /api/appliances/{id}/signatures/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.PathValue("n"))
		if err != nil {
			writeErr(w, 400, errors.New("bad signature index"))
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for i := range s.appliances {
			a := &s.appliances[i]
			if a.ID != r.PathValue("id") || n < 0 || n >= len(a.Signatures) {
				continue
			}
			a.Signatures = append(a.Signatures[:n], a.Signatures[n+1:]...)
			if len(a.Signatures) == 0 {
				s.appliances = append(s.appliances[:i], s.appliances[i+1:]...)
			}
			if err := s.saveAppliances(); err != nil {
				writeErr(w, 500, err)
				return
			}
			s.mqtt.appliancesChanged()
			writeJSONResponse(w, 200, map[string]bool{"ok": true})
			return
		}
		writeErr(w, 404, errors.New("no such appliance or signature"))
	})

	mux.HandleFunc("DELETE /api/appliances/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i := range s.appliances {
			if s.appliances[i].ID == r.PathValue("id") {
				s.appliances = append(s.appliances[:i], s.appliances[i+1:]...)
				if err := s.saveAppliances(); err != nil {
					writeErr(w, 500, err)
					return
				}
				s.mqtt.appliancesChanged()
				writeJSONResponse(w, 200, map[string]bool{"ok": true})
				return
			}
		}
		writeErr(w, 404, errors.New("no such appliance"))
	})

	// Raw per-second leg readings (diagnostics): ?from=&to= in ms (at most 6 h),
	// &format=tsv for "time P1 P2 ... Q1 Q2 ..." lines.
	mux.HandleFunc("GET /api/debug/raw", authed(func(w http.ResponseWriter, r *http.Request) {
		from, err1 := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		to, err2 := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
		if err1 != nil || err2 != nil {
			writeErr(w, 400, errors.New("from and to (unix ms) are required"))
			return
		}
		samples, err := s.rawSamples(from, to)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		if r.URL.Query().Get("format") == "tsv" {
			w.Header().Set("Content-Type", "text/tab-separated-values")
			writeRaw(w, samples)
			return
		}
		rows := make([][]float64, len(samples))
		for i, x := range samples {
			row := []float64{float64(x.At)}
			for _, v := range x.P {
				row = append(row, float64(v))
			}
			for _, v := range x.Q {
				row = append(row, float64(v))
			}
			rows[i] = row
		}
		writeJSONResponse(w, 200, map[string]any{"columns": "time, P per leg, Q per leg", "samples": rows})
	}))

	// Run the detector again over a day's recorded raw readings and replace its runs.
	mux.HandleFunc("POST /api/appliances/redetect", func(w http.ResponseWriter, r *http.Request) {
		n, err := s.redetect(r.URL.Query().Get("date"))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSONResponse(w, 200, map[string]int{"runs": n})
	})

	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSONResponse(w, 200, s.settings)
	})

	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var st Settings
		if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
			writeErr(w, 400, err)
			return
		}
		if st.Rates == nil {
			st.Rates = map[string]float64{}
		}
		for m, rate := range st.Rates {
			if _, err := time.Parse("2006-01", m); err != nil || rate < 0 || rate > 5 {
				writeErr(w, 400, fmt.Errorf("invalid rate %q: %v", m, rate))
				return
			}
		}
		if st.Mode != "" && st.Mode != "blended" && st.Mode != "monthly" {
			writeErr(w, 400, errors.New(`mode must be "blended" or "monthly"`))
			return
		}
		if st.BlendedRate != nil && (*st.BlendedRate < 0 || *st.BlendedRate > 5) {
			writeErr(w, 400, errors.New("blendedRate must be between 0 and 5 $/kWh"))
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		st.Household = s.settings.Household // edited separately (PUT /api/household)
		st.Inventory = s.settings.Inventory // and PUT /api/inventory
		if err := s.writeJSON("settings.json", st, 0o644); err != nil {
			writeErr(w, 500, err)
			return
		}
		s.settings = st
		writeJSONResponse(w, 200, st)
	})

	mux.HandleFunc("GET /api/inverters", authed(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		data, at := s.inverters.data, s.inverters.at
		s.mu.Unlock()
		if data == nil || nowMs()-at > time.Minute.Milliseconds() {
			b, err := s.gatewayGet("/api/v1/production/inverters", s.currentToken(), 25*time.Second)
			if err != nil {
				writeErr(w, 502, err)
				return
			}
			data = b
			s.mu.Lock()
			s.inverters.data, s.inverters.at = b, nowMs()
			s.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, 404, errors.New("Not found"))
	})

	// The React app; unknown paths fall back to index.html.
	files := http.FileServerFS(dist)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if p := path.Clean(r.URL.Path); p != "/" {
			if _, err := fs.Stat(dist, p[1:]); err != nil {
				r.URL.Path = "/"
			}
		}
		files.ServeHTTP(w, r)
	})

	if s.cfg.DashboardPassword == "" {
		return mux
	}
	// Optional HTTP basic auth for the whole dashboard (any username).
	want := []byte(s.cfg.DashboardPassword)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pass, ok := r.BasicAuth(); ok && subtle.ConstantTimeCompare([]byte(pass), want) == 1 {
			mux.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="Solar dashboard"`)
		http.Error(w, "Authentication required", 401)
	})
}

type PublicDay struct {
	Date                  string   `json:"date"`
	Produced              *float64 `json:"produced"`
	Consumed              *float64 `json:"consumed"`
	Imported              *float64 `json:"imported"`
	Exported              *float64 `json:"exported"`
	PeakW                 *float64 `json:"peakW"`
	PeakAt                *int64   `json:"peakAt"`
	Partial               *bool    `json:"partial"`
	Source                *string  `json:"source"`
	ProducedFromInverters bool     `json:"producedFromInverters"`
}

// publicDaily: local measurements win when they cover the whole day (or were
// backfilled); otherwise use the cloud's full-day totals if we have them.
// Caller holds s.mu.
func (s *Server) publicDaily(date string) PublicDay {
	d, c := s.daily[date], s.cloudDaily[date]
	out := PublicDay{Date: date}
	complete := d != nil && (!d.Partial || d.Before != nil)
	switch {
	case d != nil && (complete || c == nil):
		out.Produced, out.Consumed = roundPtr(d.Produced), roundPtr(d.Consumed)
		out.Imported, out.Exported = roundPtr(d.Imported), roundPtr(d.Exported)
		out.Source = ptr("gateway")
		out.ProducedFromInverters = d.ProducedFromInverters
	case c != nil:
		out.Produced, out.Consumed = roundPtr(c.Produced), roundPtr(c.Consumed)
		out.Imported, out.Exported = roundPtr(c.Imported), roundPtr(c.Exported)
		out.Source = ptr("cloud")
	}
	if d != nil {
		out.PeakW, out.PeakAt = ptr(d.PeakW), d.PeakAt
	}
	switch {
	case complete || c != nil:
		out.Partial = ptr(false)
	case d != nil:
		out.Partial = ptr(d.Partial)
	}
	return out
}
