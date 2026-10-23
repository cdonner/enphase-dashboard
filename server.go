package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	historyStep  = time.Minute      // one stored chart point per minute
	renewBefore  = time.Hour        // renew the gateway token when < 1 h is left
	partialAfter = 10 * time.Minute // a day's baseline taken after 00:10 is a partial day
	pdmEvery     = 5 * time.Minute  // inverters only report every ~5 minutes
	evEvery      = 3 * time.Hour    // EV sessions: 1 cloud call per charger

	enlightenLogin = "https://enlighten.enphaseenergy.com/login/login.json"
	entrezTokens   = "https://entrez.enphaseenergy.com/tokens"
)

type GatewayInfo struct {
	Serial    *string `json:"serial"`
	Firmware  *string `json:"firmware"`
	Reachable bool    `json:"reachable"`
}

type Credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type Sample struct {
	At           int64    `json:"at"`
	ProductionW  float64  `json:"productionW"`
	NetW         *float64 `json:"netW"` // + importing from grid, - exporting
	ConsumptionW *float64 `json:"consumptionW"`
	counters     Counters
}

type pdmEnergy struct {
	at         int64
	Production struct {
		Pcu *struct {
			WattHoursToday     float64 `json:"wattHoursToday"`
			WattHoursSevenDays float64 `json:"wattHoursSevenDays"`
		} `json:"pcu"`
	} `json:"production"`
}

type Server struct {
	cfg     Config
	gateway *http.Client // self-signed certificate
	web     *http.Client
	cloud   *Cloud
	kick    chan struct{} // trigger an immediate sample

	mu          sync.Mutex // guards everything below
	token       string
	credentials *Credentials
	gwInfo      GatewayInfo
	meters      map[int64]string // eid -> measurementType
	pdm         *pdmEnergy
	live        *Sample
	lastError   *string
	renewError  *string
	today       Today
	daily       map[string]*Day
	cloudDaily  map[string]*CloudDay
	cloudMeta   CloudMeta
	ev          EvState
	evDays      map[string]*EvDay // "serial/date" -> cached day series
	settings    Settings
	inverters   struct {
		at   int64
		data json.RawMessage
	}
	lastPersist int64
}

func NewServer(cfg Config) *Server {
	s := &Server{
		cfg: cfg,
		gateway: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			MaxIdleConns:    4,
			IdleConnTimeout: 90 * time.Second,
		}},
		web:        &http.Client{Timeout: 30 * time.Second},
		kick:       make(chan struct{}, 1),
		daily:      map[string]*Day{},
		cloudDaily: map[string]*CloudDay{},
		ev:         EvState{Chargers: []EvCharger{}},
		evDays:     map[string]*EvDay{},
		settings:   Settings{Rates: map[string]float64{}},
	}
	s.cloud = NewCloud(s)
	return s
}

func (s *Server) logf(format string, a ...any) { log.Printf(format, a...) }

func (s *Server) setError(e *string) {
	s.mu.Lock()
	s.lastError = e
	s.mu.Unlock()
}

// ---------- startup & loops ----------

func (s *Server) start() {
	var tok struct {
		Token string `json:"token"`
	}
	s.readJSON("token.json", &tok)
	var creds Credentials
	var cloudDaily struct {
		Meta CloudMeta            `json:"meta"`
		Days map[string]*CloudDay `json:"days"`
	}
	s.readJSON("cloud-daily.json", &cloudDaily)
	s.cloud.load()

	s.mu.Lock()
	s.token = strings.TrimSpace(tok.Token)
	if s.readJSON("credentials.json", &creds) && creds.Email != "" {
		s.credentials = &creds
	}
	s.readJSON("daily.json", &s.daily)
	if s.daily == nil {
		s.daily = map[string]*Day{}
	}
	s.cloudMeta = cloudDaily.Meta
	if cloudDaily.Days != nil {
		s.cloudDaily = cloudDaily.Days
	}
	s.readJSON("ev.json", &s.ev)
	s.readJSON("settings.json", &s.settings)
	if s.settings.Rates == nil {
		s.settings.Rates = map[string]float64{}
	}
	s.rollDay(dayKey(time.Now()))
	hasToken, days := s.token != "", len(s.daily)
	s.mu.Unlock()

	s.logf("Data loaded: gateway token %s, cloud %s, %d recorded days",
		map[bool]string{true: "present", false: "missing"}[hasToken],
		map[bool]string{true: "connected", false: "not connected"}[s.cloud.connected()], days)

	s.loadGatewayInfo()
	s.mu.Lock()
	if s.gwInfo.Reachable {
		s.logf("Gateway %s reachable, firmware %s", deref(s.gwInfo.Serial), deref(s.gwInfo.Firmware))
	} else if s.lastError != nil {
		s.logf("%s", *s.lastError)
	}
	s.mu.Unlock()
	s.renewIfNeeded()

	go s.sampleLoop()
	go every(10*time.Minute, 0, s.renewIfNeeded)
	go every(10*time.Minute, 10*time.Minute, s.loadGatewayInfo)
	go every(10*time.Minute, 15*time.Second, s.cloudTick) // first run after today's baseline exists
}

// every runs f after `first`, then every d.
func every(d, first time.Duration, f func()) {
	time.Sleep(first)
	f()
	for range time.Tick(d) {
		f()
	}
}

func (s *Server) sampleLoop() {
	first := true
	t := time.NewTicker(s.cfg.Poll)
	for {
		s.sample()
		s.mu.Lock()
		if first && (s.live != nil || s.lastError != nil) {
			first = false
			if s.live != nil {
				s.logf("First reading: %.0f W production", s.live.ProductionW)
			} else {
				s.logf("Reading failed: %s", *s.lastError)
			}
		}
		s.mu.Unlock()
		select {
		case <-t.C:
		case <-s.kick:
		}
	}
}

func (s *Server) kickSample() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// ---------- gateway ----------

var errUnauthorized = errors.New("Gateway rejected the token (401)")

func (s *Server) gatewayGet(path, token string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://"+s.cfg.EnvoyHost+path, nil)
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := s.gateway.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("Gateway request timed out")
		}
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == 401 {
		return nil, errUnauthorized
	}
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("Gateway returned HTTP %d", res.StatusCode)
	}
	return body, nil
}

func (s *Server) currentToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

func (s *Server) gatewayJSON(path string, timeout time.Duration, v any) error {
	b, err := s.gatewayGet(path, s.currentToken(), timeout)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

var (
	reSerial   = regexp.MustCompile(`<sn>([^<]+)</sn>`)
	reFirmware = regexp.MustCompile(`<software>([^<]+)</software>`)
)

func (s *Server) loadGatewayInfo() {
	xml, err := s.gatewayGet("/info", "", 10*time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.gwInfo.Reachable = false
		s.lastError = ptr(fmt.Sprintf("Gateway unreachable at %s: %v", s.cfg.EnvoyHost, err))
		return
	}
	info := GatewayInfo{Reachable: true}
	if m := reSerial.FindSubmatch(xml); m != nil {
		info.Serial = ptr(string(m[1]))
	}
	if m := reFirmware.FindSubmatch(xml); m != nil {
		info.Firmware = ptr(string(m[1]))
	}
	s.gwInfo = info
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---------- token handling ----------

type TokenInfo struct {
	Present   bool    `json:"present"`
	ExpiresAt *int64  `json:"expiresAt,omitempty"`
	Expired   bool    `json:"expired"`
	Role      *string `json:"role"`
	Serial    *string `json:"serial"`
}

func tokenInfo(token string) TokenInfo {
	if token == "" {
		return TokenInfo{}
	}
	info := TokenInfo{Present: true}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return info
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return info
	}
	var claims struct {
		Exp         int64  `json:"exp"`
		EnphaseUser string `json:"enphaseUser"`
		Aud         string `json:"aud"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return info
	}
	if claims.Exp > 0 {
		info.ExpiresAt = ptr(claims.Exp * 1000)
		info.Expired = *info.ExpiresAt < nowMs()
	}
	if claims.EnphaseUser != "" {
		info.Role = ptr(claims.EnphaseUser)
	}
	if claims.Aud != "" {
		info.Serial = ptr(claims.Aud)
	}
	return info
}

func (s *Server) verifyAndSaveToken(token string) error {
	token = strings.TrimSpace(token)
	if len(strings.Split(token, ".")) != 3 {
		return errors.New("That does not look like a JWT")
	}
	if _, err := s.gatewayGet("/api/v1/production", token, 25*time.Second); err != nil {
		return err
	}
	s.mu.Lock()
	s.token = token
	s.meters = nil
	s.lastError = nil
	s.mu.Unlock()
	if err := s.writeJSON("token.json", map[string]any{"token": token, "savedAt": nowMs()}, 0o600); err != nil {
		return err
	}
	s.kickSample()
	return nil
}

type httpStatusError struct {
	status int
	msg    string
}

func (e *httpStatusError) Error() string { return e.msg }

func (s *Server) requestToken(email, password string) (string, error) {
	s.mu.Lock()
	serial := deref(s.gwInfo.Serial)
	s.mu.Unlock()
	if serial == "" {
		return "", errors.New("Gateway serial number unknown - is the gateway reachable?")
	}
	res, err := s.web.PostForm(enlightenLogin, url.Values{"user[email]": {email}, "user[password]": {password}})
	if err != nil {
		return "", err
	}
	var login struct {
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
	}
	json.NewDecoder(res.Body).Decode(&login)
	res.Body.Close()
	if res.StatusCode >= 400 || login.SessionID == "" {
		msg := login.Message
		if msg == "" {
			msg = fmt.Sprintf("Enlighten login failed (HTTP %d)", res.StatusCode)
		}
		return "", &httpStatusError{401, msg}
	}
	body, _ := json.Marshal(map[string]string{"session_id": login.SessionID, "serial_num": serial, "username": email})
	res, err = s.web.Post(entrezTokens, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	token := strings.TrimSpace(string(b))
	if res.StatusCode >= 400 {
		return "", fmt.Errorf("Entrez refused the token request (HTTP %d): %.200s", res.StatusCode, token)
	}
	return token, nil
}

func (s *Server) renewIfNeeded() {
	s.mu.Lock()
	creds, info := s.credentials, tokenInfo(s.token)
	s.mu.Unlock()
	if creds == nil {
		return
	}
	if info.Present && info.ExpiresAt != nil && time.UnixMilli(*info.ExpiresAt).Sub(time.Now()) > renewBefore {
		return
	}
	token, err := s.requestToken(creds.Email, creds.Password)
	if err == nil {
		err = s.verifyAndSaveToken(token)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.renewError = ptr(err.Error())
		s.logf("Token renewal failed: %v", err)
		return
	}
	s.renewError = nil
	s.logf("Gateway token renewed")
}

// ---------- sampling ----------

func (s *Server) sample() {
	if s.currentToken() == "" {
		return
	}
	err := s.sampleOnce()
	if err != nil {
		s.setError(ptr(err.Error()))
		if errors.Is(err, errUnauthorized) {
			go s.renewIfNeeded()
		}
	}
}

func (s *Server) sampleOnce() error {
	s.mu.Lock()
	meters, pdm := s.meters, s.pdm
	s.mu.Unlock()

	if meters == nil {
		var list []struct {
			EID             int64  `json:"eid"`
			State           string `json:"state"`
			MeasurementType string `json:"measurementType"`
		}
		if err := s.gatewayJSON("/ivp/meters", 25*time.Second, &list); err != nil {
			return err
		}
		meters = map[int64]string{}
		for _, m := range list {
			if m.State == "enabled" {
				meters[m.EID] = m.MeasurementType
			}
		}
	}
	if pdm == nil || nowMs()-pdm.at > pdmEvery.Milliseconds() {
		var p pdmEnergy
		if err := s.gatewayJSON("/ivp/pdm/energy", 25*time.Second, &p); err != nil {
			return err
		}
		p.at = nowMs()
		pdm = &p
	}
	var readings []struct {
		EID           int64   `json:"eid"`
		ActivePower   float64 `json:"activePower"`
		ActEnergyDlvd float64 `json:"actEnergyDlvd"`
		ActEnergyRcvd float64 `json:"actEnergyRcvd"`
	}
	if err := s.gatewayJSON("/ivp/meters/readings", 10*time.Second, &readings); err != nil {
		return err
	}

	smp := &Sample{At: nowMs()}
	foundProd := false
	for _, r := range readings {
		switch meters[r.EID] {
		case "production":
			foundProd = true
			smp.ProductionW = math.Max(0, r.ActivePower)
			smp.counters.Produced = ptr(r.ActEnergyDlvd)
		case "net-consumption":
			smp.NetW = ptr(r.ActivePower)
			smp.counters.Imported = ptr(r.ActEnergyDlvd)
			smp.counters.Exported = ptr(r.ActEnergyRcvd)
		}
	}
	if !foundProd {
		return errors.New("No production meter reading")
	}
	if smp.NetW != nil {
		smp.ConsumptionW = ptr(math.Max(0, smp.ProductionW+*smp.NetW))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.meters, s.pdm = meters, pdm
	s.record(smp)
	s.live = smp
	s.lastError = nil
	return nil
}

// rollDay switches the in-memory chart to `date`. Caller holds s.mu.
func (s *Server) rollDay(date string) {
	if s.today.Date == date {
		return
	}
	s.today = Today{Date: date, Points: []Point{}}
	s.readJSON("history/"+date+".json", &s.today.Points)
	if s.today.Points == nil {
		s.today.Points = []Point{}
	}
}

// updateDaily recomputes today's totals from lifetime counter deltas. Caller holds s.mu.
func (s *Server) updateDaily(smp *Sample) {
	at := time.UnixMilli(smp.At)
	date := dayKey(at)
	d := s.daily[date]
	if d == nil {
		d = &Day{
			Baseline:   smp.counters,
			BaselineAt: smp.At,
			Partial:    at.Sub(startOfDay(at)) > partialAfter,
		}
		s.daily[date] = d
	}
	// d.Before: energy between midnight and BaselineAt, backfilled from the cloud.
	var before Energy
	if d.Before != nil {
		before = *d.Before
	}
	withBefore := func(cur, base *float64, b float64) *float64 {
		if cur == nil || base == nil {
			return nil
		}
		return ptr(math.Max(0, *cur-*base) + b)
	}
	d.Produced = withBefore(smp.counters.Produced, d.Baseline.Produced, before.Produced)
	d.Imported = withBefore(smp.counters.Imported, d.Baseline.Imported, before.Imported)
	d.Exported = withBefore(smp.counters.Exported, d.Baseline.Exported, before.Exported)
	d.Consumed = nil
	if d.Produced != nil && d.Imported != nil && d.Exported != nil {
		d.Consumed = ptr(*d.Produced + *d.Imported - *d.Exported)
	}
	// When we missed midnight, the meter deltas only cover part of the day. The inverters'
	// own wattHoursToday (the meter "today" fields are always 0 on some firmware) still
	// gives the full day's production.
	d.ProducedFromInverters = false
	if d.Partial && d.Before == nil && s.pdm != nil && s.pdm.Production.Pcu != nil &&
		dayKey(time.UnixMilli(s.pdm.at)) == date && d.Produced != nil && s.pdm.Production.Pcu.WattHoursToday > *d.Produced {
		d.Produced = ptr(s.pdm.Production.Pcu.WattHoursToday)
		d.ProducedFromInverters = true
	}
	if smp.ProductionW > d.PeakW {
		d.PeakW = smp.ProductionW
		d.PeakAt = ptr(smp.At)
	}
}

// record adds a sample to today's totals and chart. Caller holds s.mu.
func (s *Server) record(smp *Sample) {
	date := dayKey(time.UnixMilli(smp.At))
	s.rollDay(date)
	s.updateDaily(smp)

	pts := s.today.Points
	if len(pts) == 0 || smp.At-int64(*pts[len(pts)-1][0]) >= historyStep.Milliseconds() {
		s.today.Points = append(pts, Point{ptr(float64(smp.At)), ptr(math.Round(smp.ProductionW)), roundPtr(smp.ConsumptionW), roundPtr(smp.NetW)})
	}
	if smp.At-s.lastPersist >= historyStep.Milliseconds() {
		s.lastPersist = smp.At
		if err := s.writeJSON("history/"+date+".json", s.today.Points, 0o644); err != nil {
			s.logf("Saving history: %v", err)
		}
		if err := s.writeJSON("daily.json", s.daily, 0o644); err != nil {
			s.logf("Saving daily totals: %v", err)
		}
	}
}

// ---------- cloud backfill ----------

func (s *Server) saveCloudDaily() {
	err := s.writeJSON("cloud-daily.json", map[string]any{"meta": s.cloudMeta, "days": s.cloudDaily}, 0o644)
	if err != nil {
		s.logf("Saving cloud data: %v", err)
	}
}

// syncCloudDaily fetches full-day totals for the last 30 days: 4 API calls, once a
// day after 03:00 (so yesterday is final), or right away the first time.
func (s *Server) syncCloudDaily() error {
	today := dayKey(time.Now())
	s.mu.Lock()
	synced := s.cloudMeta.DailySyncedFor
	s.mu.Unlock()
	if synced == today || (synced != "" && time.Now().Hour() < 3) {
		return nil
	}
	days, err := s.cloud.dailyTotals(dayKey(time.Now().AddDate(0, 0, -30)))
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range days {
		s.cloudDaily[k] = v
	}
	s.cloudMeta.DailySyncedFor = today
	s.saveCloudDaily()
	return nil
}

// backfillToday fills in today's energy and chart before the dashboard started from
// 15-minute cloud intervals: 4 API calls, at most once a day.
func (s *Server) backfillToday() error {
	date := dayKey(time.Now())
	s.mu.Lock()
	d := s.daily[date]
	if d == nil || !d.Partial || d.Before != nil || s.cloudMeta.TodayAttempted == date {
		s.mu.Unlock()
		return nil
	}
	s.cloudMeta.TodayAttempted = date
	s.saveCloudDaily()
	baselineAt := d.BaselineAt
	s.mu.Unlock()

	rows, err := s.cloud.intervals(startOfDay(time.Now()).Unix())
	if err != nil {
		return err
	}
	cut := float64(baselineAt) / 1000
	reached := false
	for _, r := range rows {
		if float64(r.End) >= cut {
			reached = true
		}
	}
	if !reached {
		return errors.New("Cloud data does not reach the dashboard start yet")
	}
	var before Energy
	var points []Point
	w := func(wh *float64) *float64 { // Wh per 15 min -> average W
		if wh == nil {
			return nil
		}
		return ptr(*wh * 4)
	}
	val := func(v *float64) float64 {
		if v == nil {
			return 0
		}
		return *v
	}
	for _, r := range rows {
		frac := math.Max(0, math.Min(1, (cut-float64(r.End-900))/900)) // share of interval before baseline
		if frac == 0 {
			continue
		}
		before.Produced += val(r.Produced) * frac
		before.Imported += val(r.Imported) * frac
		before.Exported += val(r.Exported) * frac
		if frac == 1 {
			var net *float64
			if r.Imported != nil && r.Exported != nil {
				net = w(ptr(*r.Imported - *r.Exported))
			}
			points = append(points, Point{ptr(float64((r.End - 450) * 1000)), w(r.Produced), w(r.Consumed), net})
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	d.Before = &before
	if err := s.writeJSON("daily.json", s.daily, 0o644); err != nil {
		return err
	}
	if s.today.Date == date {
		first := math.Inf(1)
		if len(s.today.Points) > 0 {
			first = *s.today.Points[0][0]
		}
		merged := []Point{}
		for _, p := range points {
			if *p[0] < first {
				merged = append(merged, p)
			}
		}
		s.today.Points = append(merged, s.today.Points...)
		return s.writeJSON("history/"+date+".json", s.today.Points, 0o644)
	}
	return nil
}

// syncEv refreshes EV charger sessions: 1 call per charger every 3 hours,
// plus 1 to list chargers once a day.
func (s *Server) syncEv() error {
	s.mu.Lock()
	ev := s.ev
	s.mu.Unlock()
	if nowMs()-ev.At < evEvery.Milliseconds() {
		return nil
	}
	known, listedAt := ev.Chargers, ev.ListedAt
	if nowMs()-ev.ListedAt >= 24*time.Hour.Milliseconds() {
		var err error
		if known, err = s.cloud.evChargers(); err != nil {
			return err
		}
		listedAt = nowMs()
	}
	chargers := []EvCharger{}
	for _, c := range known {
		sessions, err := s.cloud.evSessions(c.Serial, 20)
		if err != nil {
			return err
		}
		c.Sessions = sessions
		chargers = append(chargers, c)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ev = EvState{At: nowMs(), ListedAt: listedAt, Chargers: chargers}
	return s.writeJSON("ev.json", s.ev, 0o644)
}

func (s *Server) cloudTick() {
	if !s.cloud.connected() {
		return
	}
	err := s.syncCloudDaily()
	if err == nil {
		err = s.backfillToday()
	}
	if err == nil {
		err = s.syncEv()
	}
	if err == nil {
		err = s.syncEvDays()
	}
	s.cloud.setError(err)
	if err != nil {
		s.logf("Cloud sync: %v", err)
	}
}

// ---------- EV cost: solar/grid split ----------

const evDaysPerTick = 8 // backfill a few days per cloud tick (3 calls each)

// evDay returns the cached series for serial/date, loading it from disk if needed.
// Caller holds s.mu.
func (s *Server) evDay(serial, date string) *EvDay {
	key := serial + "/" + date
	if d, ok := s.evDays[key]; ok {
		return d
	}
	var d EvDay
	if !s.readJSON("ev-days/"+key+".json", &d) {
		return nil
	}
	s.evDays[key] = &d
	return &d
}

// syncEvDays fetches the 15-minute series for completed days that have charging sessions.
func (s *Server) syncEvDays() error {
	today := dayKey(time.Now())
	type want struct{ serial, date string }
	var missing []want
	seen := map[string]bool{}
	s.mu.Lock()
	for _, c := range s.ev.Chargers {
		for _, sess := range c.Sessions {
			if sess.End == nil {
				continue
			}
			for t := startOfDay(time.UnixMilli(sess.Start)); !t.After(time.UnixMilli(*sess.End)); t = t.AddDate(0, 0, 1) {
				date := dayKey(t)
				key := c.Serial + "/" + date
				if date >= today || seen[key] {
					continue
				}
				seen[key] = true
				if s.evDay(c.Serial, date) == nil {
					missing = append(missing, want{c.Serial, date})
				}
			}
		}
	}
	s.mu.Unlock()

	for i, w := range missing {
		if i == evDaysPerTick {
			break
		}
		t, _ := time.ParseInLocation("2006-01-02", w.date, time.Local)
		day, err := s.cloud.evDay(w.serial, t.Unix())
		if err != nil {
			return err
		}
		if err := s.writeJSON("ev-days/"+w.serial+"/"+w.date+".json", day, 0o644); err != nil {
			return err
		}
		s.mu.Lock()
		s.evDays[w.serial+"/"+w.date] = day
		s.mu.Unlock()
	}
	return nil
}

type EvSessionView struct {
	EvSession
	SolarKwh    *float64 `json:"solarKwh"`
	EnphaseCost *float64 `json:"enphaseCost"`
	CostSource  string   `json:"costSource"` // "rates": kWh × your monthly rate; "enphase": Enphase's figure
}

// sessionView prices a session at the normal grid rate for its month (what it would
// cost without solar) and, once the 15-minute series are cached, works out how much of
// it solar supplied: per interval, in proportion to how much of the household's
// consumption solar covered. Caller holds s.mu.
func (s *Server) sessionView(serial string, sess EvSession) EvSessionView {
	v := EvSessionView{EvSession: sess, EnphaseCost: sess.Cost, CostSource: "enphase"}
	if sess.Kwh == nil {
		return v
	}
	if rate := s.settings.rateFor(time.UnixMilli(sess.Start).Format("2006-01")); rate != nil {
		v.Cost = ptr(math.Round(*sess.Kwh**rate*100) / 100)
		v.CostSource = "rates"
	}
	if sess.End == nil || *sess.Kwh <= 0 {
		return v
	}
	start, end := sess.Start/1000, *sess.End/1000
	var evWh, solarWh float64
	for e := (start/900 + 1) * 900; e-900 < end; e += 900 {
		day := s.evDay(serial, dayKey(time.Unix(e-1, 0)))
		if day == nil {
			return v // series not fetched yet
		}
		ev := day.EV[e]
		if ev <= 0 {
			continue
		}
		evWh += ev
		solarWh += ev * math.Min(1, day.Prod[e]/math.Max(day.Cons[e], ev)) // solar share of this interval's load
	}
	if evWh > 0 {
		v.SolarKwh = ptr(*sess.Kwh * solarWh / evWh)
	}
	return v
}
