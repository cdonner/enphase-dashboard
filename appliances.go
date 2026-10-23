package main

// Appliance view (stage 1 + naming): runs from the detector are stored per day,
// matched against appliances the user has named, grouped into activities for the day
// timeline, and unnamed runs are clustered by signature so they can be named in batches.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	activityGap      = 20 * time.Minute // repeats of one signature closer than this are one activity
	clusterDays      = 7                // unnamed runs from this many days are clustered
	matchTolPct      = 0.15             // signatures match within 15% per leg...
	matchTolW        = 60.0             // ...or 60 W, whichever is larger
	minClusterRuns   = 1                // show signatures for naming from their first run
	maxClusters      = 40               // most energy first
	appliancesFile   = "appliances.json"
	maxRunsPerDayMem = 50_000
)

// Sig is one step signature: power and reactive power per leg, with an optional label
// naming what it is within its appliance (e.g. "warm-up", "printing").
type Sig struct {
	DP    []float64 `json:"dp"`
	DQ    []float64 `json:"dq"`
	Label string    `json:"label,omitempty"`
}

// Appliance is a name the user gave to one or more signatures (e.g. a laser printer's
// warm-up, fuser and motor).
type Appliance struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Signatures []Sig  `json:"signatures"`
	// Single signature as stored before appliances could have several; migrated on load.
	DP []float64 `json:"dp,omitempty"`
	DQ []float64 `json:"dq,omitempty"`
}

// migrateAppliances converts the old one-signature form. Caller holds s.mu.
func (s *Server) migrateAppliances() {
	for i := range s.appliances {
		a := &s.appliances[i]
		if len(a.Signatures) == 0 && len(a.DP) > 0 {
			a.Signatures = []Sig{{DP: a.DP, DQ: a.DQ}}
		}
		a.DP, a.DQ = nil, nil
	}
}

// addSignature names a signature: it joins the appliance with that name (case-insensitive)
// or creates one. Caller holds s.mu.
func (s *Server) addSignature(name string, sig Sig) Appliance {
	for i := range s.appliances {
		a := &s.appliances[i]
		if strings.EqualFold(a.Name, name) {
			for _, have := range a.Signatures {
				if sameSignature(have.DP, sig.DP) {
					return *a
				}
			}
			a.Signatures = append(a.Signatures, sig)
			return *a
		}
	}
	a := Appliance{ID: newID(), Name: name, Signatures: []Sig{sig}}
	s.appliances = append(s.appliances, a)
	return a
}

type Signature struct {
	DP        []float64 `json:"dp"`
	DQ        []float64 `json:"dq"`
	PowerW    float64   `json:"powerW"`
	Legs      string    `json:"legs"`      // "240 V", "120 V · leg 1", "240 V + leg 2", "3-phase", ...
	Character string    `json:"character"` // "resistive", "motor", "mixed"
}

func describe(dp, dq []float64) Signature {
	s := Signature{DP: dp, DQ: dq}
	var used []int
	var p, q float64
	for i, v := range dp {
		p += v
		q += dq[i]
		if v > edgeThreshold/2 {
			used = append(used, i)
		}
	}
	s.PowerW = p
	switch {
	case len(dp) == 1:
		s.Legs = "single phase"
	case len(dp) == 2 && len(used) == 2:
		ratio := dp[0] / dp[1]
		switch {
		case ratio > 0.7 && ratio < 1.43:
			s.Legs = "240 V"
		case ratio >= 1.43:
			s.Legs = "240 V + leg 1"
		default:
			s.Legs = "240 V + leg 2"
		}
	case len(dp) == 2 && len(used) == 1:
		s.Legs = fmt.Sprintf("120 V · leg %d", used[0]+1)
	case len(used) == 1:
		s.Legs = fmt.Sprintf("phase %d", used[0]+1)
	default:
		s.Legs = fmt.Sprintf("%d phases", len(used))
	}
	pf := math.Abs(p) / math.Hypot(p, q)
	switch {
	case pf >= 0.97:
		s.Character = "resistive"
	case pf >= 0.85:
		s.Character = "mixed"
	default:
		s.Character = "motor"
	}
	return s
}

// sameSignature: same legs used, each within tolerance.
func sameSignature(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if (a[i] > edgeThreshold/2) != (b[i] > edgeThreshold/2) {
			return false
		}
		if math.Abs(a[i]-b[i]) > math.Max(matchTolW, matchTolPct*math.Max(math.Abs(a[i]), math.Abs(b[i]))) {
			return false
		}
	}
	return true
}

// ---------- storage ----------

type dayRuns struct {
	Runs []Run `json:"runs"`
}

// loadDayRuns returns the stored runs of a day. Caller holds s.mu.
func (s *Server) loadDayRuns(date string) []Run {
	if date == s.events.date {
		return s.events.runs
	}
	var d dayRuns
	s.readJSON("events/"+date+".json", &d)
	return d.Runs
}

// recordRuns stores newly closed runs under the day they started. Caller holds s.mu.
func (s *Server) recordRuns(runs []Run) {
	today := dayKey(time.Now())
	if s.events.date != today {
		s.events.date, s.events.runs = today, s.loadDayRuns(today)
		s.events.dirty = false
	}
	for _, r := range runs {
		date := dayKey(time.UnixMilli(r.Start))
		if date == today {
			if len(s.events.runs) < maxRunsPerDayMem {
				s.events.runs = append(s.events.runs, r)
				s.events.dirty = true
			}
			continue
		}
		// Started on an earlier day (e.g. ran past midnight): append to that day's file.
		old := dayRuns{Runs: s.loadDayRuns(date)}
		old.Runs = append(old.Runs, r)
		if err := s.writeJSON("events/"+date+".json", old, 0o644); err != nil {
			s.logf("Saving events: %v", err)
		}
	}
}

// persistEvents writes today's runs if they changed, and the detector state. Caller holds s.mu.
func (s *Server) persistEvents() {
	if err := s.writeJSON("detector.json", s.detector.State(nowMs()), 0o644); err != nil {
		s.logf("Saving detector state: %v", err)
	}
	if !s.events.dirty {
		return
	}
	if err := s.writeJSON("events/"+s.events.date+".json", dayRuns{Runs: s.events.runs}, 0o644); err != nil {
		s.logf("Saving events: %v", err)
		return
	}
	s.events.dirty = false
}

func (s *Server) saveAppliances() error {
	return s.writeJSON(appliancesFile, s.appliances, 0o644)
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// applianceFor returns the named appliance matching a run, if any. Caller holds s.mu.
func (s *Server) applianceFor(r Run) *Appliance {
	a, _ := s.matchSignature(r.DP)
	return a
}

// matchSignature returns the named appliance and the index of its signature matching dp.
// Caller holds s.mu.
func (s *Server) matchSignature(dp []float64) (*Appliance, int) {
	for i := range s.appliances {
		for n, sig := range s.appliances[i].Signatures {
			if sameSignature(sig.DP, dp) {
				return &s.appliances[i], n
			}
		}
	}
	return nil, -1
}

// moveSignature takes signature n off appliance id and names it `name` (an existing
// appliance or a new one). An appliance left without signatures is removed. Caller holds s.mu.
func (s *Server) moveSignature(id string, n int, name string) (Appliance, error) {
	for i := range s.appliances {
		a := &s.appliances[i]
		if a.ID != id || n < 0 || n >= len(a.Signatures) {
			continue
		}
		sig := a.Signatures[n]
		if strings.EqualFold(a.Name, name) {
			return *a, nil
		}
		a.Signatures = append(a.Signatures[:n], a.Signatures[n+1:]...)
		if len(a.Signatures) == 0 {
			s.appliances = append(s.appliances[:i], s.appliances[i+1:]...)
		}
		return s.addSignature(name, sig), nil
	}
	return Appliance{}, errors.New("no such appliance or signature")
}

// ---------- day view ----------

type Activity struct {
	Start       int64     `json:"start"`
	End         *int64    `json:"end"` // nil while running
	Runs        int       `json:"runs"`
	EnergyWh    float64   `json:"energyWh"`
	OnSeconds   float64   `json:"onSeconds"`
	Signature   Signature `json:"signature"`
	Appliance   *string   `json:"appliance"`   // name, if identified
	ApplianceID *string   `json:"applianceId"` // id, if identified
	ClusterKey  string    `json:"clusterKey"`
	Spans       []Span    `json:"spans"` // the individual runs, for the timeline
	lastEnd     int64
	running     bool
}

// Span is one run within an activity.
type Span struct {
	Start int64     `json:"s"`
	End   int64     `json:"e"`
	Sig   int       `json:"sig"`             // index into the appliance's signatures, -1 if unnamed
	Label string    `json:"label,omitempty"` // that signature's label
	Desc  Signature `json:"desc"`            // this run's own signature
	// Already on when monitoring started: the real start is earlier than S.
	StartedBefore bool `json:"startedBefore,omitempty"`
}

// activities groups a day's runs (plus runs still on) into activities: same appliance
// or signature, with gaps shorter than activityGap. Caller holds s.mu.
func (s *Server) activities(runs []Run, open []Run, now int64) []Activity {
	all := append(append([]Run{}, runs...), open...)
	sort.Slice(all, func(i, j int) bool { return all[i].Start < all[j].Start })
	out := []Activity{}
	for _, r := range all {
		end := now
		if r.End != nil {
			end = *r.End
		}
		app, sigN := s.matchSignature(r.DP)
		var name, id *string
		label := ""
		if app != nil {
			name, id, label = &app.Name, &app.ID, app.Signatures[sigN].Label
		}
		// Extend the latest activity with the same appliance/signature, if close enough.
		var a *Activity
		for i := len(out) - 1; i >= 0; i-- {
			c := &out[i]
			cEnd := c.lastEnd
			if c.running {
				cEnd = now
			}
			same := (name != nil && c.Appliance != nil && *c.Appliance == *name) ||
				(name == nil && c.Appliance == nil && sameSignature(c.Signature.DP, r.DP))
			if same && r.Start-cEnd <= activityGap.Milliseconds() {
				a = c
				break
			}
		}
		if a == nil {
			out = append(out, Activity{Start: r.Start, Signature: describe(r.DP, r.DQ), Appliance: name, ApplianceID: id})
			a = &out[len(out)-1]
		}
		a.Runs++
		a.OnSeconds += float64(end-r.Start) / 1000
		a.EnergyWh += r.power() * float64(end-r.Start) / 3_600_000
		a.Spans = append(a.Spans, Span{Start: r.Start, End: end, Sig: sigN, Label: label, Desc: describe(r.DP, r.DQ), StartedBefore: r.StartedBefore})
		a.lastEnd = max(a.lastEnd, end)
		a.running = a.running || r.End == nil
	}
	for i := range out {
		if !out[i].running {
			out[i].End = ptr(out[i].lastEnd)
		}
		out[i].ClusterKey = clusterKey(out[i].Signature.DP)
	}
	return out
}

// ---------- clusters of unnamed runs ----------

type Cluster struct {
	Key         string        `json:"key"`
	Signature   Signature     `json:"signature"`
	Runs        int           `json:"runs"`
	Days        int           `json:"days"`
	EnergyWh    float64       `json:"energyWh"`
	MedianOnSec float64       `json:"medianOnSec"`
	Examples    []int64       `json:"examples"`     // start times of a few runs (ms)
	Suggestions []string      `json:"suggestions"`  // likely appliances from the built-in catalog
	AI          *AISuggestion `json:"ai,omitempty"` // from the last "Ask AI", if any

	// Extra features used by the AI suggestions.
	Hours        [24]int `json:"hours"`        // runs started per hour of day
	ShortOnSec   float64 `json:"shortOnSec"`   // 10th percentile on-time
	LongOnSec    float64 `json:"longOnSec"`    // 90th percentile on-time
	MedianGapSec float64 `json:"medianGapSec"` // typical time between runs (0 if unknown)
}

// clusterKey is a stable id for a signature: legs used and power rounded on a log scale.
func clusterKey(dp []float64) string {
	parts := make([]string, len(dp))
	for i, v := range dp {
		if v <= edgeThreshold/2 {
			parts[i] = "0"
			continue
		}
		parts[i] = fmt.Sprint(int(math.Round(math.Log(v) / math.Log(1.25))))
	}
	return strings.Join(parts, ".")
}

// clusters groups the unnamed runs of the last clusterDays days by signature (greedy:
// each run joins the first cluster whose centroid it matches). Caller holds s.mu.
func (s *Server) clusters() []Cluster {
	type acc struct {
		dp, dq  []float64
		n       int
		energy  float64
		days    map[string]bool
		ons     []float64
		samples []int64
		starts  []int64
		hours   [24]int
	}
	var groups []*acc
	for d := 0; d < clusterDays; d++ {
		date := dayKey(time.Now().AddDate(0, 0, -d))
		for _, r := range s.loadDayRuns(date) {
			if r.End == nil || s.applianceFor(r) != nil {
				continue
			}
			var g *acc
			for _, c := range groups {
				if sameSignature(c.dp, r.DP) {
					g = c
					break
				}
			}
			if g == nil {
				g = &acc{dp: append([]float64{}, r.DP...), dq: append([]float64{}, r.DQ...), days: map[string]bool{}}
				groups = append(groups, g)
			}
			// Running mean keeps the centroid representative.
			for k := range r.DP {
				g.dp[k] += (r.DP[k] - g.dp[k]) / float64(g.n+1)
				g.dq[k] += (r.DQ[k] - g.dq[k]) / float64(g.n+1)
			}
			g.n++
			g.energy += r.energyWh()
			g.days[date] = true
			g.ons = append(g.ons, float64(*r.End-r.Start)/1000)
			if len(g.samples) < 5 {
				g.samples = append(g.samples, r.Start)
			}
			g.starts = append(g.starts, r.Start)
			g.hours[time.UnixMilli(r.Start).Hour()]++
		}
	}
	out := []Cluster{}
	for _, g := range groups {
		if g.n < minClusterRuns {
			continue
		}
		sort.Float64s(g.ons)
		sig, median := describe(g.dp, g.dq), g.ons[len(g.ons)/2]
		c := Cluster{
			Key: clusterKey(g.dp), Signature: sig, Runs: g.n, Days: len(g.days),
			EnergyWh: g.energy, MedianOnSec: median, Examples: g.samples, Suggestions: suggest(sig, median),
			Hours: g.hours, ShortOnSec: g.ons[len(g.ons)/10], LongOnSec: g.ons[len(g.ons)*9/10],
		}
		sort.Slice(g.starts, func(i, j int) bool { return g.starts[i] < g.starts[j] })
		var gaps []float64
		for i := 1; i < len(g.starts); i++ {
			gaps = append(gaps, float64(g.starts[i]-g.starts[i-1])/1000)
		}
		if len(gaps) > 0 {
			sort.Float64s(gaps)
			c.MedianGapSec = gaps[len(gaps)/2]
		}
		if ai, ok := s.aiSuggestions[c.Key]; ok {
			c.AI = &ai
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EnergyWh > out[j].EnergyWh })
	if len(out) > maxClusters {
		out = out[:maxClusters]
	}
	return out
}
