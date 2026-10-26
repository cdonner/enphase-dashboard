package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"time"
)

// On-disk formats (data/*.json) are shared with the earlier Node version of the server.

type Counters struct {
	Produced *float64 `json:"produced"`
	Imported *float64 `json:"imported"`
	Exported *float64 `json:"exported"`
}

type Energy struct {
	Produced float64 `json:"produced"`
	Imported float64 `json:"imported"`
	Exported float64 `json:"exported"`
}

// Day is one local calendar day as measured from the gateway's meters.
type Day struct {
	Baseline              Counters `json:"baseline"`   // lifetime counters at the first sample of the day
	BaselineAt            int64    `json:"baselineAt"` // ms
	Partial               bool     `json:"partial"`    // baseline taken well after midnight
	Before                *Energy  `json:"before,omitempty"`
	PeakW                 float64  `json:"peakW"`
	PeakAt                *int64   `json:"peakAt"`
	Produced              *float64 `json:"produced"`
	Imported              *float64 `json:"imported"`
	Exported              *float64 `json:"exported"`
	Consumed              *float64 `json:"consumed"`
	ProducedFromInverters bool     `json:"producedFromInverters"`
}

// CloudDay is a full-day total from the Enphase cloud.
type CloudDay struct {
	Produced *float64 `json:"produced"`
	Consumed *float64 `json:"consumed"`
	Imported *float64 `json:"imported"`
	Exported *float64 `json:"exported"`
}

type CloudMeta struct {
	DailySyncedFor string `json:"dailySyncedFor,omitempty"`
	TodayAttempted string `json:"todayAttempted,omitempty"`
}

// Point is one chart point: [ms, productionW, consumptionW, netW].
type Point [4]*float64

type Today struct {
	Date   string  `json:"date"`
	Points []Point `json:"points"`
}

type EvSession struct {
	Start int64    `json:"start"`
	End   *int64   `json:"end"`
	Kwh   *float64 `json:"kwh"`
	Miles *float64 `json:"miles"`
	Cost  *float64 `json:"cost"`
}

type EvCharger struct {
	Serial       string      `json:"serial"`
	Name         string      `json:"name"`
	Model        string      `json:"model"`
	Status       string      `json:"status"`
	Active       bool        `json:"active"`
	LastReportAt *int64      `json:"lastReportAt"`
	Sessions     []EvSession `json:"sessions,omitempty"`
}

type EvState struct {
	At       int64       `json:"at"`
	ListedAt int64       `json:"listedAt"`
	Chargers []EvCharger `json:"chargers"`
}

// ---------- helpers ----------

func ptr[T any](v T) *T { return &v }

func nowMs() int64 { return time.Now().UnixMilli() }

func dayKey(t time.Time) string { return t.Local().Format("2006-01-02") }

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Local().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

func roundPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	return ptr(math.Round(*v))
}

func (s *Server) readJSON(name string, v any) bool {
	b, err := os.ReadFile(filepath.Join(s.cfg.DataDir, name))
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

// writeJSON writes atomically (temp file + rename).
func (s *Server) writeJSON(name string, v any, mode os.FileMode) error {
	target := filepath.Join(s.cfg.DataDir, name)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

func (s *Server) removeFile(name string) {
	err := os.Remove(filepath.Join(s.cfg.DataDir, name))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.logf("Cannot remove %s: %v", name, err)
	}
}

// EvDay holds one day's 15-minute series (Wh, keyed by interval end in unix seconds):
// charger draw, solar production and household consumption. Cached in ev-days/.
type EvDay struct {
	EV   map[int64]float64 `json:"ev"`
	Prod map[int64]float64 `json:"prod"`
	Cons map[int64]float64 `json:"cons"`
}

// Settings are user preferences edited in the UI (data/settings.json). See rates.go.
type Settings struct {
	Mode        string             `json:"mode,omitempty"`        // "blended" or "monthly"
	BlendedRate *float64           `json:"blendedRate,omitempty"` // $/kWh; nil = average of the last 12 months
	Rates       map[string]float64 `json:"rates"`                 // "YYYY-MM" -> $/kWh
	Household   string             `json:"household,omitempty"`   // owner's description, for AI suggestions
	Inventory   *Inventory         `json:"inventory,omitempty"`   // owner's appliance list, for suggestions (hints.go)
}
