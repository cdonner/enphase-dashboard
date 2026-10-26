package main

// Teach mode and clean slate.
//
// Teach: the owner switches one appliance on and off a few times while the dashboard
// watches, then the detector is run over that window's raw readings. Steps are grouped by
// signature; the one that switched most often is offered as the appliance's. Loads cycling
// on their own (fridge, furnace) rarely line up with the owner's on/off rounds.
//
// Clean slate: forget every name and the recorded runs, so learning starts over. The old
// files are moved to a backup folder, not deleted.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	teachWarmup = time.Minute      // readings before the window, so stable levels are known
	teachMax    = 30 * time.Minute // longest teach window
)

// TeachCandidate is one signature that switched during a teach window.
type TeachCandidate struct {
	Signature Signature `json:"signature"`
	Ons       int       `json:"ons"`                 // switch-ons in the window
	Offs      int       `json:"offs"`                // switch-offs in the window
	Times     []int64   `json:"times"`               // when it switched (ms), on or off
	Appliance string    `json:"appliance,omitempty"` // already named as this
}

type teachGroup struct {
	dp, dq    []float64 // sums, divided by n for the mean
	n         int
	ons, offs int
	times     []int64
}

func (g *teachGroup) sig() Sig {
	dp, dq := make([]float64, len(g.dp)), make([]float64, len(g.dq))
	for i := range dp {
		dp[i], dq[i] = g.dp[i]/float64(g.n), g.dq[i]/float64(g.n)
	}
	return Sig{DP: dp, DQ: dq}
}

// teach finds the signatures that switched between from and to (ms), most switches first.
func (s *Server) teach(from, to int64) ([]TeachCandidate, error) {
	if to <= from || to-from > teachMax.Milliseconds() {
		return nil, fmt.Errorf("the teach window must be positive and at most %s", teachMax)
	}
	samples, err := s.rawSamples(from-teachWarmup.Milliseconds(), to)
	if err != nil {
		return nil, err
	}
	if len(samples) == 0 {
		return nil, errors.New("no readings recorded in that time")
	}
	d := NewDetector()
	var runs []Run
	for _, x := range samples {
		p, q := make([]float64, len(x.P)), make([]float64, len(x.Q))
		for i := range x.P {
			p[i], q[i] = float64(x.P[i]), float64(x.Q[i])
		}
		d.Add(x.At, p, q)
		runs = append(runs, d.drainClosed()...)
	}
	if d.pending != nil {
		d.flush() // a step in the last seconds
	}
	runs = append(runs, d.drainClosed()...)
	runs = append(runs, d.open...)

	var groups []*teachGroup
	for _, r := range runs {
		on := r.Start >= from && !r.StartedBefore
		off := r.End != nil && *r.End >= from
		if !on && !off {
			continue
		}
		var g *teachGroup
		for _, c := range groups {
			if sameSignature(c.sig(), Sig{DP: r.DP, DQ: r.DQ}) {
				g = c
				break
			}
		}
		if g == nil {
			g = &teachGroup{dp: make([]float64, len(r.DP)), dq: make([]float64, len(r.DQ))}
			groups = append(groups, g)
		}
		for i := range r.DP {
			g.dp[i] += r.DP[i]
			if i < len(r.DQ) && i < len(g.dq) {
				g.dq[i] += r.DQ[i]
			}
		}
		g.n++
		if on {
			g.ons++
			g.times = append(g.times, r.Start)
		}
		if off {
			g.offs++
			g.times = append(g.times, *r.End)
		}
	}

	out := make([]TeachCandidate, 0, len(groups))
	s.mu.Lock()
	for _, g := range groups {
		sig := g.sig()
		sort.Slice(g.times, func(i, j int) bool { return g.times[i] < g.times[j] })
		c := TeachCandidate{Signature: describe(sig.DP, sig.DQ), Ons: g.ons, Offs: g.offs, Times: g.times}
		if a, _ := s.matchSignature(sig.DP, sig.DQ); a != nil {
			c.Appliance = a.Name
		}
		out = append(out, c)
	}
	s.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Ons+a.Offs != b.Ons+b.Offs {
			return a.Ons+a.Offs > b.Ons+b.Offs
		}
		return a.Signature.PowerW > b.Signature.PowerW
	})
	return out, nil
}

// cleanSlate forgets all appliance names, signature labels, ignored signatures, AI
// suggestions and recorded runs. They are moved to backups/clean-slate-<time>/. The
// owner's appliance list, household notes and raw readings stay. Returns the backup folder.
func (s *Server) cleanSlate() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := "clean-slate-" + time.Now().Format("2006-01-02-150405")
	backup := filepath.Join(s.cfg.DataDir, "backups", name)
	if err := os.MkdirAll(backup, 0o755); err != nil {
		return "", err
	}
	for _, f := range []string{appliancesFile, aiFile, "events"} {
		err := os.Rename(filepath.Join(s.cfg.DataDir, f), filepath.Join(backup, f))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	s.appliances = []Appliance{}
	s.aiSuggestions = map[string]AISuggestion{}
	s.events.runs, s.events.dirty = nil, false
	s.pastDays = nil
	s.clusterCache.at = 0
	s.mqtt.appliancesChanged()
	return filepath.Join("backups", name), nil
}
