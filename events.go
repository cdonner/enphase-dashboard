package main

// Appliance event detection (non-intrusive load monitoring, stage 1: event log).
//
// Every second the gateway reports real and reactive power per leg (phase) for the
// production and net-consumption meters; consumption per leg = net + production, so
// solar fluctuations cancel out. Per leg we track the last stable level and emit an
// edge when a new stable level differs by more than edgeThreshold. Edges on different
// legs within mergeWindow are one event (a 240 V appliance switches both legs). An "on"
// event opens a run; an "off" event closes the open run it matches, or splits off part
// of a larger one (e.g. a sauna heater dropping one of its elements). Repeated runs of
// the same signature are grouped into activities for display (activities.go).

import (
	"math"
	"time"
)

const (
	edgeThreshold = 60.0            // W: smallest step reported
	stableSamples = 2               // consecutive readings that must agree to count as stable
	stableTolW    = 25.0            // W: allowed spread within a stable window...
	stableTolPct  = 0.03            // ...or this share of the level, whichever is larger
	mergeWindow   = 2 * time.Second // edges on different legs this close are one event
	maxOpenRuns   = 64
)

// Run is one appliance (or one stage of one) switched on, and off again unless End is nil.
type Run struct {
	Start int64     `json:"start"` // ms
	End   *int64    `json:"end,omitempty"`
	DP    []float64 `json:"dp"` // W per leg
	DQ    []float64 `json:"dq"` // var per leg
	// Leg levels just before it switched on (for the consistency check). Only kept while
	// the run is open; saved with the detector state, not with closed runs.
	Base []float64 `json:"base,omitempty"`
	// The load was already on when monitoring started, so Start is when it was first seen.
	StartedBefore bool `json:"startedBefore,omitempty"`
}

func (r Run) power() float64 {
	var p float64
	for _, v := range r.DP {
		p += v
	}
	return p
}

func (r Run) energyWh() float64 {
	if r.End == nil {
		return 0
	}
	return r.power() * float64(*r.End-r.Start) / 3_600_000
}

type legTracker struct {
	p, q       []float64 // recent samples
	stableP    float64
	stableQ    float64
	haveStable bool
	moveAt     int64 // ms of the first sample that left the stable level
}

type pendingEdge struct {
	at     int64
	dp, dq []float64
	base   []float64 // leg levels before the step
	moved  []bool    // legs that contributed to the step
}

type Detector struct {
	legs    []*legTracker
	pending *pendingEdge // edges are held for mergeWindow so legs can be combined
	open    []Run
	closed  []Run // closed since the last drain
	edges   int   // edges seen today

	// Lowest stable level per leg in each of the last 24 hours, to know the base load.
	hourMin  [][24]float64
	hourSeen [][24]int64 // unix hour each bucket belongs to
	started  bool        // the first full set of stable levels has been checked
	firstAt  int64       // time of the first reading since this start (ms)
}

func NewDetector() *Detector { return &Detector{} }

// DetectorState is saved every minute so a restart (e.g. an update) can carry on.
type DetectorState struct {
	SavedAt  int64         `json:"savedAt"`
	Open     []Run         `json:"open"`
	HourMin  [][24]float64 `json:"hourMin"`
	HourSeen [][24]int64   `json:"hourSeen"`
}

const (
	restoreOpenWithin = 10 * time.Minute // open runs survive a restart this short
	alreadyOnMinW     = 1000.0           // excess over the base load reported at startup
)

func (d *Detector) State(now int64) DetectorState {
	open := d.open
	if open == nil {
		open = []Run{}
	}
	return DetectorState{SavedAt: now, Open: open, HourMin: d.hourMin, HourSeen: d.hourSeen}
}

// Restore picks up a saved state: the base-load history always, the open runs only if
// the state is recent.
func (d *Detector) Restore(st DetectorState, now int64) {
	d.hourMin, d.hourSeen = st.HourMin, st.HourSeen
	if now-st.SavedAt <= restoreOpenWithin.Milliseconds() {
		d.open = st.Open
		d.started = len(st.Open) > 0 // they already account for what's on
	}
}

// baseLoad is a leg's lowest stable level in the last 24 hours (ok=false if unknown).
func (d *Detector) baseLoad(leg int, now int64) (float64, bool) {
	if leg >= len(d.hourMin) {
		return 0, false
	}
	hour := now / 3_600_000
	best, ok := math.Inf(1), false
	for i := 0; i < 24; i++ {
		if d.hourSeen[leg][i] > 0 && hour-d.hourSeen[leg][i] < 24 {
			best, ok = math.Min(best, d.hourMin[leg][i]), true
		}
	}
	return best, ok
}

// trackBase records stable levels per hour, and once at startup reports a large load that
// is already on (the excess over the base load) as a run, so its switch-off can close it.
func (d *Detector) trackBase(at int64) {
	n := len(d.legs)
	if len(d.hourMin) != n {
		d.hourMin, d.hourSeen = make([][24]float64, n), make([][24]int64, n)
	}
	hour := at / 3_600_000
	allStable := true
	for k, l := range d.legs {
		if !l.haveStable {
			allStable = false
			continue
		}
		i := hour % 24
		if d.hourSeen[k][i] != hour || l.stableP < d.hourMin[k][i] {
			if d.hourSeen[k][i] != hour {
				d.hourSeen[k][i] = hour
				d.hourMin[k][i] = l.stableP
			}
			d.hourMin[k][i] = math.Min(d.hourMin[k][i], l.stableP)
		}
	}
	if d.started || !allStable {
		return
	}
	d.started = true
	dp, dq, base := make([]float64, n), make([]float64, n), make([]float64, n)
	var excess float64
	for k, l := range d.legs {
		b, ok := d.baseLoad(k, at-3_600_000) // history from before this start
		if !ok {
			return // no history yet (first start)
		}
		if l.stableP-b > edgeThreshold {
			dp[k], base[k] = l.stableP-b, b
			excess += dp[k]
		} else {
			base[k] = l.stableP
		}
	}
	if excess >= alreadyOnMinW {
		d.open = append(d.open, Run{Start: at, DP: dp, DQ: dq, Base: base, StartedBefore: true})
	}
}

// Add feeds one reading: per-leg consumption P (W) and Q (var) at time `at` (ms).
func (d *Detector) Add(at int64, p, q []float64) {
	if d.firstAt == 0 {
		d.firstAt = at
	}
	if len(d.legs) != len(p) {
		d.legs = make([]*legTracker, len(p))
		for i := range d.legs {
			d.legs[i] = &legTracker{}
		}
	}
	if d.pending != nil && at-d.pending.at > mergeWindow.Milliseconds() {
		d.flush()
	}
	for i, l := range d.legs {
		dp, dq, moveAt, ok := l.add(at, p[i], q[i])
		if !ok {
			continue
		}
		if d.pending == nil {
			n := len(p)
			d.pending = &pendingEdge{at: moveAt, dp: make([]float64, n), dq: make([]float64, n),
				base: make([]float64, n), moved: make([]bool, n)}
		}
		if !d.pending.moved[i] {
			d.pending.moved[i], d.pending.base[i] = true, l.stableP-dp
		}
		d.pending.dp[i] += dp
		d.pending.dq[i] += dq
		d.pending.at = min(d.pending.at, moveAt)
	}
	if d.pending == nil {
		d.closeVanished(at)
		d.trackBase(at)
	}
}

// closeVanished closes open runs whose load is no longer there even though no matching
// off step was seen: every leg the run uses has settled back to (about) the level it had
// before the run started. This catches switch-offs that were missed, e.g. a 60 W load
// that drops by slightly less than edgeThreshold.
func (d *Detector) closeVanished(at int64) {
	for i := len(d.open) - 1; i >= 0; i-- {
		r := d.open[i]
		if len(r.Base) != len(d.legs) {
			continue
		}
		gone := true
		for k, l := range d.legs {
			if r.DP[k] <= edgeThreshold/2 {
				continue
			}
			if !l.haveStable || l.stableP > r.Base[k]+0.4*r.DP[k] {
				gone = false
				break
			}
		}
		if gone {
			end := at
			r.End, r.Base = &end, nil
			d.closed = append(d.closed, r)
			d.open = append(d.open[:i], d.open[i+1:]...)
		}
	}
}

// add returns a step (dp, dq) when the leg settles at a new stable level.
func (l *legTracker) add(at int64, p, q float64) (dp, dq float64, moveAt int64, ok bool) {
	l.p = append(l.p, p)
	l.q = append(l.q, q)
	if len(l.p) > stableSamples {
		l.p, l.q = l.p[1:], l.q[1:]
	}
	if l.haveStable && math.Abs(p-l.stableP) > edgeThreshold && l.moveAt == 0 {
		l.moveAt = at
	}
	if len(l.p) < stableSamples {
		return
	}
	lo, hi, sumP, sumQ := l.p[0], l.p[0], 0.0, 0.0
	for i, v := range l.p {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
		sumP, sumQ = sumP+v, sumQ+l.q[i]
	}
	meanP, meanQ := sumP/stableSamples, sumQ/stableSamples
	if hi-lo > math.Max(stableTolW, stableTolPct*math.Abs(meanP)) {
		return // still changing
	}
	switch {
	case !l.haveStable:
		l.stableP, l.stableQ, l.haveStable = meanP, meanQ, true
	case math.Abs(meanP-l.stableP) > edgeThreshold:
		dp, dq, moveAt, ok = meanP-l.stableP, meanQ-l.stableQ, l.moveAt, true
		if moveAt == 0 {
			moveAt = at - int64(stableSamples-1)*1000
		}
		l.stableP, l.stableQ = meanP, meanQ
	default:
		// Follow slow drift without reporting it.
		l.stableP += (meanP - l.stableP) * 0.1
		l.stableQ += (meanQ - l.stableQ) * 0.1
	}
	l.moveAt = 0
	return
}

// flush turns the pending edge into an on or off event.
func (d *Detector) flush() {
	e := d.pending
	d.pending = nil
	d.edges++
	var sum float64
	for _, v := range e.dp {
		sum += v
	}
	if sum > 0 {
		base := make([]float64, len(e.dp))
		for k, l := range d.legs {
			if e.moved[k] {
				base[k] = e.base[k]
			} else {
				base[k] = l.stableP
			}
		}
		d.open = append(d.open, Run{Start: e.at, DP: e.dp, DQ: e.dq, Base: base})
		if len(d.open) > maxOpenRuns {
			d.open = d.open[1:] // never matched an off; forget the oldest
		}
		return
	}
	d.matchOff(e)
}

// matchOff closes the open run that best matches an off edge: an exact match (same size
// on every leg, within tolerance), else part of a larger run on the same legs.
func (d *Detector) matchOff(e *pendingEdge) {
	off := make([]float64, len(e.dp))
	var offSum float64
	for i, v := range e.dp {
		off[i] = -v
		offSum += -v
	}
	tol := math.Max(edgeThreshold, 0.2*offSum)

	best, bestErr, partial := -1, math.Inf(1), false
	for i := len(d.open) - 1; i >= 0; i-- {
		r := d.open[i]
		if len(r.DP) != len(off) {
			continue
		}
		var mismatch float64
		fits := true // off fits inside r: no bigger on any leg, and only on legs r uses
		for k := range off {
			mismatch += math.Abs(r.DP[k] - off[k])
			if off[k] > r.DP[k]+tol/2 || (off[k] > edgeThreshold && r.DP[k] <= edgeThreshold) {
				fits = false
			}
		}
		if mismatch <= tol && mismatch < bestErr {
			best, bestErr, partial = i, mismatch, false
		} else if best < 0 && fits && r.power() > offSum+tol {
			best, partial = i, true
		}
	}
	if best < 0 {
		// An off without a matching on: started before we were watching, or merged with
		// another step. A big one is recorded from the start of monitoring, marked as such.
		if offSum >= alreadyOnMinW && d.firstAt > 0 {
			end := e.at
			d.closed = append(d.closed, Run{Start: d.firstAt, End: &end, DP: off,
				DQ: make([]float64, len(off)), StartedBefore: true})
		}
		return
	}
	r := d.open[best]
	end := e.at
	if !partial {
		r.End, r.Base = &end, nil
		d.closed = append(d.closed, r)
		d.open = append(d.open[:best], d.open[best+1:]...)
		return
	}
	// Split: the part that switched off becomes a closed run; the rest stays on.
	part := Run{Start: r.Start, End: &end, DP: off, DQ: make([]float64, len(off)), StartedBefore: r.StartedBefore}
	share := offSum / r.power()
	for k := range r.DQ {
		part.DQ[k] = r.DQ[k] * share
		d.open[best].DQ[k] -= part.DQ[k]
		d.open[best].DP[k] -= off[k]
	}
	d.closed = append(d.closed, part)
}

// drainClosed returns runs closed since the last call.
func (d *Detector) drainClosed() []Run {
	c := d.closed
	d.closed = nil
	return c
}
