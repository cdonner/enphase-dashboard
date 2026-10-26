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
	maxLegLag     = 8 * time.Second // longest wait for a slower leg of the same event
	maxOpenRuns   = 64

	// A step this small relative to a big run on the same legs adjusts that run instead of
	// being a load of its own: an EV charger changing its current, a heater's level drifting.
	adjustFrac   = 0.05
	adjustMinRun = 2000.0 // W
	// The open runs may claim more than a leg draws above its base load for this long
	// before the best-fitting one is closed (its off step was missed).
	balanceHold = time.Minute
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
	at     int64 // when the first leg started moving
	seen   int64 // when the first leg settled (the merge window runs from here)
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

	overSince []int64 // per leg: when the open runs started claiming more than the leg draws
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
	orphanWithin      = 12 * time.Hour   // an unmatched big switch-off this soon after a start is recorded
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
	if d.pending != nil && at-d.pending.seen > mergeWindow.Milliseconds() && !d.otherLegMoving(at) {
		d.flush()
	}
	for i, l := range d.legs {
		dp, dq, moveAt, ok := l.add(at, p[i], q[i])
		if !ok {
			continue
		}
		if d.pending == nil {
			n := len(p)
			d.pending = &pendingEdge{at: moveAt, seen: at, dp: make([]float64, n), dq: make([]float64, n),
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
		d.checkBalance(at)
		d.trackBase(at)
	}
}

// checkBalance (Hart's conservation check): the open runs on a leg can't add up to more
// than the leg draws above its base load. If they do for balanceHold, an off step was
// missed (e.g. it merged with another step); the run that best explains the excess is
// closed as of when the excess began.
func (d *Detector) checkBalance(at int64) {
	if len(d.overSince) != len(d.legs) {
		d.overSince = make([]int64, len(d.legs))
	}
	for k, l := range d.legs {
		base, ok := d.baseLoad(k, at)
		if !l.haveStable || !ok {
			d.overSince[k] = 0
			continue
		}
		var claimed float64
		for _, r := range d.open {
			if k < len(r.DP) && r.DP[k] > 0 {
				claimed += r.DP[k]
			}
		}
		excess := claimed - (l.stableP - base)
		if excess <= math.Max(edgeThreshold, 0.1*claimed) {
			d.overSince[k] = 0
			continue
		}
		if d.overSince[k] == 0 {
			d.overSince[k] = at
			continue
		}
		if at-d.overSince[k] < balanceHold.Milliseconds() {
			continue
		}
		best, bestErr := -1, math.Inf(1)
		for i, r := range d.open {
			if k >= len(r.DP) || r.DP[k] <= edgeThreshold/2 || r.DP[k] > excess+math.Max(edgeThreshold, 0.2*r.DP[k]) {
				continue
			}
			if e := math.Abs(r.DP[k] - excess); e < bestErr {
				best, bestErr = i, e
			}
		}
		if best >= 0 {
			r := d.open[best]
			end := d.overSince[k]
			r.End, r.Base = &end, nil
			d.closed = append(d.closed, r)
			d.open = append(d.open[:best], d.open[best+1:]...)
		}
		d.overSince[k] = 0
	}
}

// adjusts returns the open run a small step belongs to as a change of level (see
// adjustFrac), or -1. step is the size per leg, positive for on and off alike.
func (d *Detector) adjusts(step []float64) int {
	var sum float64
	for _, v := range step {
		sum += v
	}
	best, bestP := -1, 0.0
	for i, r := range d.open {
		p := r.power()
		if len(r.DP) != len(step) || p < adjustMinRun || sum > adjustFrac*p || p <= bestP {
			continue
		}
		fits := true
		for k := range step {
			if (step[k] > edgeThreshold/4) != (r.DP[k] > edgeThreshold/2) || math.Abs(step[k]/sum-r.DP[k]/p) > 0.25 {
				fits = false
				break
			}
		}
		if fits {
			best, bestP = i, p
		}
	}
	return best
}

// adjust changes an open run's level by dp/dq (an adjustment step, see adjusts).
func (d *Detector) adjust(i int, dp, dq []float64) {
	r := &d.open[i]
	for k := range dp {
		r.DP[k] += dp[k]
		if k < len(r.DQ) && k < len(dq) {
			r.DQ[k] += dq[k]
		}
	}
}

// otherLegMoving: a leg not yet part of the pending edge started moving at about the same
// time and hasn't settled - e.g. one leg of a 240 V heater ramping a few seconds longer.
// The edge waits for it, for at most maxLegLag.
func (d *Detector) otherLegMoving(at int64) bool {
	e := d.pending
	if at-e.seen > maxLegLag.Milliseconds() {
		return false
	}
	for i, l := range d.legs {
		if !e.moved[i] && l.moveAt != 0 && abs64(l.moveAt-e.at) <= mergeWindow.Milliseconds() {
			return true
		}
	}
	return false
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
	size := make([]float64, len(e.dp))
	for k, v := range e.dp {
		size[k] = math.Abs(v)
	}
	if i := d.adjusts(size); i >= 0 && !d.exactOff(e) {
		d.adjust(i, e.dp, e.dq)
		return
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

// exactOff: an off step that matches an open run exactly (it switches off a load of its
// own rather than adjusting a bigger one).
func (d *Detector) exactOff(e *pendingEdge) bool {
	var offSum float64
	for _, v := range e.dp {
		offSum -= v
	}
	if offSum <= 0 {
		return false
	}
	tol := math.Max(edgeThreshold, 0.2*offSum)
	for _, r := range d.open {
		if len(r.DP) != len(e.dp) {
			continue
		}
		var mismatch float64
		for k := range e.dp {
			mismatch += math.Abs(r.DP[k] + e.dp[k])
		}
		if mismatch <= tol {
			return true
		}
	}
	return false
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
	splitFrom, splitSize := -1, math.Inf(1)
	for i := len(d.open) - 1; i >= 0; i-- {
		r := d.open[i]
		if len(r.DP) != len(off) {
			continue
		}
		var mismatch float64
		for k := range off {
			mismatch += math.Abs(r.DP[k] - off[k])
		}
		if mismatch <= tol && mismatch < bestErr {
			best, bestErr = i, mismatch
		} else if r.power() > offSum+tol && splitFits(off, offSum, r) && r.power() < splitSize {
			splitFrom, splitSize = i, r.power()
		}
	}
	if best < 0 && splitFrom >= 0 {
		best, partial = splitFrom, true
	}
	if best < 0 {
		// An off without a matching on: started before we were watching, or merged with
		// another step. A big one is recorded from the start of monitoring, marked as such.
		if offSum >= alreadyOnMinW && d.firstAt > 0 && e.at-d.firstAt < orphanWithin.Milliseconds() {
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

// splitFits: an off step may be split off a larger run only if it looks like part of it -
// the same legs (a 240 V heater element drops on both legs, a 120 V load on one) in about
// the same proportions, and no bigger than the run on any leg. Otherwise an unrelated
// small switch-off would be carved out of e.g. a running sauna, which then never closes.
func splitFits(off []float64, offSum float64, r Run) bool {
	rp := r.power()
	for k := range off {
		if (off[k] > edgeThreshold) != (r.DP[k] > edgeThreshold) || off[k] > r.DP[k]+edgeThreshold {
			return false
		}
		if math.Abs(off[k]/offSum-r.DP[k]/rp) > 0.2 {
			return false
		}
	}
	return true
}

// drainClosed returns runs closed since the last call.
func (d *Detector) drainClosed() []Run {
	c := d.closed
	d.closed = nil
	return c
}
