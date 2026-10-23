package main

import (
	"math"
	"testing"
)

// feed runs a per-second sequence of per-leg power levels through a detector.
// q is reactive power as a fraction of p (0 = resistive).
func feed(d *Detector, start int64, seconds int, level func(t int) []float64, qFrac float64) {
	for t := 0; t < seconds; t++ {
		p := level(t)
		q := make([]float64, len(p))
		for i := range p {
			q[i] = p[i] * qFrac
		}
		d.Add((start+int64(t))*1000, p, q)
	}
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestDetector120V(t *testing.T) {
	d := NewDetector()
	// Base 370/300 W; a 1.2 kW microwave on leg 2 from t=10 to t=130, with ±5 W noise.
	feed(d, 0, 200, func(t int) []float64 {
		p := []float64{370 + float64(t%3), 300 - float64(t%2)*5}
		if t >= 10 && t < 130 {
			p[1] += 1200
		}
		return p
	}, 0)
	d.Add(201_000, []float64{370, 300}, []float64{0, 0}) // flush
	d.Add(205_000, []float64{370, 300}, []float64{0, 0})
	runs := d.drainClosed()
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1: %+v", len(runs), runs)
	}
	r := runs[0]
	if !near(r.DP[1], 1200, 30) || !near(r.DP[0], 0, 30) {
		t.Errorf("dp = %v", r.DP)
	}
	if dur := (*r.End - r.Start) / 1000; dur < 118 || dur > 122 {
		t.Errorf("duration %d s, want ~120", dur)
	}
	if sig := describe(r.DP, r.DQ); sig.Legs != "120 V · leg 2" || sig.Character != "resistive" {
		t.Errorf("signature %+v", sig)
	}
}

func TestDetectorSaunaStages(t *testing.T) {
	d := NewDetector()
	// 240 V heater: 3 elements of 2 kW (1 kW per leg each) on at t=10; the thermostat
	// drops one element at t=600 and another at t=900; the last goes off at t=1200.
	feed(d, 0, 1300, func(t int) []float64 {
		n := 0.0
		switch {
		case t >= 10 && t < 600:
			n = 3
		case t >= 600 && t < 900:
			n = 2
		case t >= 900 && t < 1200:
			n = 1
		}
		return []float64{400 + 1000*n, 300 + 1000*n}
	}, 0)
	runs := d.drainClosed()
	if len(runs) != 3 || len(d.open) != 0 {
		t.Fatalf("got %d closed, %d open, want 3 closed: %+v", len(runs), len(d.open), runs)
	}
	var energy float64
	for _, r := range runs {
		if sig := describe(r.DP, r.DQ); sig.Legs != "240 V" || !near(sig.PowerW, 2000, 60) {
			t.Errorf("run %+v -> %+v", r, sig)
		}
		energy += r.energyWh()
	}
	// 2 kW for 590 s + 890 s + 1190 s = 2670 s -> 1483 Wh
	if !near(energy, 1483, 30) {
		t.Errorf("energy %.0f Wh, want ~1483", energy)
	}
}

func TestDetectorTwoStepAndNoise(t *testing.T) {
	d := NewDetector()
	// A furnace blower ramping up in two steps (+160, +250 W, inductive), noise ±20 W.
	feed(d, 0, 400, func(t int) []float64 {
		p := 500.0 + float64((t*7)%41) - 20
		switch {
		case t >= 50 && t < 51:
			p += 160
		case t >= 51 && t < 300:
			p += 410
		}
		return []float64{p, 300}
	}, 1.0) // power factor 0.71, typical of a small motor
	runs := d.drainClosed()
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1: %+v", len(runs), runs)
	}
	if sig := describe(runs[0].DP, runs[0].DQ); sig.Character != "motor" || !near(sig.PowerW, 410, 40) {
		t.Errorf("signature %+v", sig)
	}
}

func TestSignatureMatchAndClusterKey(t *testing.T) {
	if !sameSignature([]float64{0, 1200}, []float64{0, 1260}) {
		t.Error("1200 vs 1260 W on leg 2 should match")
	}
	if sameSignature([]float64{0, 1200}, []float64{1200, 0}) {
		t.Error("different legs must not match")
	}
	if sameSignature([]float64{2000, 2000}, []float64{2000, 0}) {
		t.Error("240 V vs 120 V must not match")
	}
	if clusterKey([]float64{0, 1200}) != clusterKey([]float64{0, 1210}) {
		t.Error("cluster key should be stable for small differences")
	}
}

func TestSuggestions(t *testing.T) {
	cases := []struct {
		dp, dq []float64
		onSec  float64
		want   string
	}{
		{[]float64{2250, 2250}, []float64{0, 0}, 1800, "Electric water heater"},
		{[]float64{0, 1150}, []float64{0, 300}, 120, "Microwave"},
		{[]float64{0, 390}, []float64{0, 5}, 3, "Laser printer (fuser warming)"},
		{[]float64{1500, 1500}, []float64{1500, 1500}, 900, "Air conditioner / heat pump"},
		{[]float64{350, 0}, []float64{350, 0}, 900, "Furnace blower"},
	}
	for _, c := range cases {
		got := suggest(describe(c.dp, c.dq), c.onSec)
		found := false
		for _, g := range got {
			found = found || g == c.want
		}
		if !found {
			t.Errorf("%v %.0fs: suggestions %v, want %q among them", c.dp, c.onSec, got, c.want)
		}
	}
}
