package main

// Enphase cloud (API v4, "Watt" plan) - used only for what the local gateway can't
// provide: full-day totals for past days, the part of today before the dashboard
// started, and EV charger sessions. The plan allows ~10 calls/minute and
// 1,000/month, so calls are serialized and kept to a handful per day.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	cloudAPI    = "https://api.enphaseenergy.com/api/v4"
	cloudOAuth  = "https://api.enphaseenergy.com/oauth"
	redirectURI = "https://api.enphaseenergy.com/oauth/redirect_uri"
	minCallGap  = 7 * time.Second
)

// cloudConfig is data/cloud.json. App credentials may instead come from the
// environment (env wins); OAuth tokens and the system id are always kept in the file.
type cloudConfig struct {
	APIKey            string `json:"apiKey,omitempty"`
	ClientID          string `json:"clientId,omitempty"`
	ClientSecret      string `json:"clientSecret,omitempty"`
	AccessToken       string `json:"accessToken,omitempty"`
	RefreshToken      string `json:"refreshToken,omitempty"`
	AccessExpiresAt   int64  `json:"accessExpiresAt,omitempty"`
	RefreshObtainedAt int64  `json:"refreshObtainedAt,omitempty"`
	SystemID          int64  `json:"systemId,omitempty"`
}

type Cloud struct {
	s        *Server
	client   *http.Client
	call     sync.Mutex // serializes API calls (rate limit)
	lastCall time.Time

	mu         sync.Mutex // guards cfg and status
	cfg        cloudConfig
	lastSyncAt *int64
	err        *string
}

func NewCloud(s *Server) *Cloud {
	return &Cloud{s: s, client: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Cloud) load() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.s.readJSON("cloud.json", &c.cfg)
	for _, e := range []struct {
		v   string
		dst *string
	}{{c.s.cfg.APIKey, &c.cfg.APIKey}, {c.s.cfg.ClientID, &c.cfg.ClientID}, {c.s.cfg.ClientSecret, &c.cfg.ClientSecret}} {
		if e.v != "" {
			*e.dst = e.v
		}
	}
}

// save writes cloud.json. Caller holds c.mu.
func (c *Cloud) save() error { return c.s.writeJSON("cloud.json", c.cfg, 0o600) }

func (c *Cloud) configuredLocked() bool {
	return c.cfg.APIKey != "" && c.cfg.ClientID != "" && c.cfg.ClientSecret != ""
}

func (c *Cloud) configured() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.configuredLocked()
}

func (c *Cloud) connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.configuredLocked() && c.cfg.RefreshToken != ""
}

func (c *Cloud) setError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.err = nil
	} else {
		c.err = ptr(err.Error())
	}
}

type CloudStatus struct {
	Configured   bool    `json:"configured"`
	Connected    bool    `json:"connected"`
	LastSyncAt   *int64  `json:"lastSyncAt"`
	Error        *string `json:"error"`
	AuthorizeURL *string `json:"authorizeUrl"`
}

func (c *Cloud) status() CloudStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := CloudStatus{
		Configured: c.configuredLocked(),
		Connected:  c.configuredLocked() && c.cfg.RefreshToken != "",
		LastSyncAt: c.lastSyncAt,
		Error:      c.err,
	}
	if c.cfg.ClientID != "" {
		st.AuthorizeURL = ptr(cloudOAuth + "/authorize?response_type=code&client_id=" + url.QueryEscape(c.cfg.ClientID) +
			"&redirect_uri=" + url.QueryEscape(redirectURI))
	}
	return st
}

// oauthToken runs a token request and stores the result. Caller holds c.mu.
func (c *Cloud) oauthToken(params url.Values) error {
	req, _ := http.NewRequest("POST", cloudOAuth+"/token?"+params.Encode(), nil)
	req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)
	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var body struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode >= 400 || body.AccessToken == "" {
		switch {
		case body.ErrorDescription != "":
			return errors.New(body.ErrorDescription)
		case body.Error != "":
			return errors.New(body.Error)
		}
		return fmt.Errorf("OAuth HTTP %d", res.StatusCode)
	}
	c.cfg.AccessToken = body.AccessToken
	c.cfg.RefreshToken = body.RefreshToken
	c.cfg.AccessExpiresAt = nowMs() + body.ExpiresIn*1000
	c.cfg.RefreshObtainedAt = nowMs()
	return c.save()
}

// connect exchanges the code shown on the redirect page after the owner approves access.
func (c *Cloud) connect(code string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.configuredLocked() {
		return errors.New("Set ENPHASE_API_KEY, ENPHASE_CLIENT_ID and ENPHASE_CLIENT_SECRET first")
	}
	if err := c.oauthToken(url.Values{
		"grant_type": {"authorization_code"}, "redirect_uri": {redirectURI}, "code": {strings.TrimSpace(code)},
	}); err != nil {
		return err
	}
	c.err = nil
	return nil
}

func (c *Cloud) accessToken() (token, apiKey string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.configuredLocked() || c.cfg.RefreshToken == "" {
		return "", "", errors.New("Enphase cloud not connected")
	}
	if c.cfg.AccessToken == "" || c.cfg.AccessExpiresAt-nowMs() < 10*time.Minute.Milliseconds() {
		if err := c.oauthToken(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.cfg.RefreshToken}}); err != nil {
			return "", "", err
		}
	}
	return c.cfg.AccessToken, c.cfg.APIKey, nil
}

// get is a serialized, rate-limited GET against the v4 API.
func (c *Cloud) get(path string, v any) error {
	c.call.Lock()
	defer c.call.Unlock()
	if wait := time.Until(c.lastCall.Add(minCallGap)); wait > 0 {
		time.Sleep(wait)
	}
	c.lastCall = time.Now()
	token, key, err := c.accessToken()
	if err != nil {
		return err
	}
	req, _ := http.NewRequest("GET", cloudAPI+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("key", key)
	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		var e struct{ Details, Message string }
		json.Unmarshal(b, &e)
		msg := e.Details
		if msg == "" {
			msg = e.Message
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return fmt.Errorf("Enphase cloud %s: %s", strings.SplitN(path, "?", 2)[0], msg)
	}
	return json.Unmarshal(b, v)
}

func (c *Cloud) systemID() (int64, error) {
	c.mu.Lock()
	id := c.cfg.SystemID
	c.mu.Unlock()
	if id != 0 {
		return id, nil
	}
	var body struct {
		Systems []struct {
			SystemID int64 `json:"system_id"`
		} `json:"systems"`
	}
	if err := c.get("/systems", &body); err != nil {
		return 0, err
	}
	if len(body.Systems) == 0 {
		return 0, errors.New("No systems on this Enphase account")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.SystemID = body.Systems[0].SystemID
	return c.cfg.SystemID, c.save()
}

func (c *Cloud) markSynced() {
	c.mu.Lock()
	c.lastSyncAt = ptr(nowMs())
	c.mu.Unlock()
}

// dailyTotals returns full-day totals (Wh) from startDate through the last complete day. 4 calls.
func (c *Cloud) dailyTotals(startDate string) (map[string]*CloudDay, error) {
	id, err := c.systemID()
	if err != nil {
		return nil, err
	}
	q := "?start_date=" + startDate
	var prod struct {
		StartDate  string     `json:"start_date"`
		Production []*float64 `json:"production"`
	}
	var cons struct {
		Consumption []*float64 `json:"consumption"`
	}
	var imp struct {
		Import []*float64 `json:"import"`
	}
	var exp struct {
		Export []*float64 `json:"export"`
	}
	for _, r := range []struct {
		path string
		v    any
	}{{"energy_lifetime", &prod}, {"consumption_lifetime", &cons}, {"energy_import_lifetime", &imp}, {"energy_export_lifetime", &exp}} {
		if err := c.get(fmt.Sprintf("/systems/%d/%s%s", id, r.path, q), r.v); err != nil {
			return nil, err
		}
	}
	at := func(a []*float64, i int) *float64 {
		if i < len(a) {
			return a[i]
		}
		return nil
	}
	start, err := time.ParseInLocation("2006-01-02", prod.StartDate, time.Local)
	if err != nil {
		return nil, fmt.Errorf("Enphase cloud: unexpected start_date %q", prod.StartDate)
	}
	out := map[string]*CloudDay{}
	for i, wh := range prod.Production {
		out[start.AddDate(0, 0, i).Format("2006-01-02")] = &CloudDay{
			Produced: wh, Consumed: at(cons.Consumption, i), Imported: at(imp.Import, i), Exported: at(exp.Export, i),
		}
	}
	c.markSynced()
	return out, nil
}

type Interval struct {
	End                                    int64
	Produced, Consumed, Imported, Exported *float64
}

// flatIntervals decodes an "intervals" array that may be nested one level ([[...]]).
func flatIntervals(raw json.RawMessage) []map[string]json.Number {
	var items []json.RawMessage
	json.Unmarshal(raw, &items)
	var out []map[string]json.Number
	for _, it := range items {
		if t := bytes.TrimSpace(it); len(t) > 0 && t[0] == '[' {
			out = append(out, flatIntervals(it)...)
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(it))
		dec.UseNumber()
		var anyMap map[string]any
		if dec.Decode(&anyMap) == nil {
			m := map[string]json.Number{}
			for k, v := range anyMap {
				if n, ok := v.(json.Number); ok {
					m[k] = n
				}
			}
			out = append(out, m)
		}
	}
	return out
}

// intervals returns 15-minute intervals (Wh) since startAt (unix seconds), oldest first. 4 calls.
func (c *Cloud) intervals(startAt int64) ([]Interval, error) {
	id, err := c.systemID()
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf("?start_at=%d&granularity=day", startAt)
	byEnd := map[int64]*Interval{}
	for _, r := range []struct {
		path, field string
		dst         func(*Interval) **float64
	}{
		{"telemetry/production_meter", "wh_del", func(i *Interval) **float64 { return &i.Produced }},
		{"telemetry/consumption_meter", "enwh", func(i *Interval) **float64 { return &i.Consumed }},
		{"energy_import_telemetry", "wh_imported", func(i *Interval) **float64 { return &i.Imported }},
		{"energy_export_telemetry", "wh_exported", func(i *Interval) **float64 { return &i.Exported }},
	} {
		var body struct {
			Intervals json.RawMessage `json:"intervals"`
		}
		if err := c.get(fmt.Sprintf("/systems/%d/%s%s", id, r.path, q), &body); err != nil {
			return nil, err
		}
		for _, m := range flatIntervals(body.Intervals) {
			end, err := m["end_at"].Int64()
			if err != nil {
				continue
			}
			row := byEnd[end]
			if row == nil {
				row = &Interval{End: end}
				byEnd[end] = row
			}
			if n, ok := m[r.field]; ok {
				if f, err := n.Float64(); err == nil {
					*r.dst(row) = ptr(f)
				}
			}
		}
	}
	out := make([]Interval, 0, len(byEnd))
	for _, r := range byEnd {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].End < out[j].End })
	c.markSynced()
	return out, nil
}

// evChargers lists the system's EV chargers (IQ EV Charger reports to the cloud only). 1 call.
func (c *Cloud) evChargers() ([]EvCharger, error) {
	id, err := c.systemID()
	if err != nil {
		return nil, err
	}
	var body struct {
		Devices struct {
			EvChargers []struct {
				SerialNumber string `json:"serial_number"`
				Name         string `json:"name"`
				Model        string `json:"model"`
				Status       string `json:"status"`
				Active       bool   `json:"active"`
				LastReportAt int64  `json:"last_report_at"`
			} `json:"ev_chargers"`
		} `json:"devices"`
	}
	if err := c.get(fmt.Sprintf("/systems/%d/ev_charger/devices", id), &body); err != nil {
		return nil, err
	}
	out := []EvCharger{}
	for _, d := range body.Devices.EvChargers {
		ch := EvCharger{Serial: d.SerialNumber, Name: d.Name, Model: d.Model, Status: d.Status, Active: d.Active}
		if d.LastReportAt > 0 {
			ch.LastReportAt = ptr(d.LastReportAt * 1000)
		}
		out = append(out, ch)
	}
	return out, nil
}

// evSessions returns recent charging sessions, newest first. 1 call.
func (c *Cloud) evSessions(serial string, limit int) ([]EvSession, error) {
	id, err := c.systemID()
	if err != nil {
		return nil, err
	}
	var body struct {
		Sessions []struct {
			StartTime   int64    `json:"start_time"`
			EndTime     *int64   `json:"end_time"`
			EnergyAdded *float64 `json:"energy_added"`
			MilesAdded  *float64 `json:"miles_added"`
			Cost        *float64 `json:"cost"`
		} `json:"sessions"`
	}
	path := fmt.Sprintf("/systems/%d/ev_charger/%s/sessions?limit=%d", id, url.PathEscape(serial), limit)
	if err := c.get(path, &body); err != nil {
		return nil, err
	}
	out := []EvSession{}
	for _, s := range body.Sessions {
		ev := EvSession{Start: s.StartTime * 1000, Kwh: s.EnergyAdded, Miles: s.MilesAdded, Cost: s.Cost}
		if s.EndTime != nil && *s.EndTime > 0 {
			ev.End = ptr(*s.EndTime * 1000)
		}
		out = append(out, ev)
	}
	return out, nil
}
