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
		chargers := []chargerView{}
		for _, c := range s.ev.Chargers {
			cv := chargerView{EvCharger: c, Sessions: []EvSessionView{}}
			for _, sess := range c.Sessions {
				cv.Sessions = append(cv.Sessions, s.sessionView(c.Serial, sess))
			}
			chargers = append(chargers, cv)
		}
		writeJSONResponse(w, 200, map[string]any{"at": s.ev.At, "chargers": chargers, "settings": s.settings})
	}))

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
		if err := s.writeJSON("settings.json", st, 0o644); err != nil {
			writeErr(w, 500, err)
			return
		}
		s.mu.Lock()
		s.settings = st
		s.mu.Unlock()
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
