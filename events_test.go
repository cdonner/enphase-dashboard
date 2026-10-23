package main

import (
	"math"
	"strings"
	"testing"
	"time"
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
		{[]float64{0, 390}, []float64{0, 5}, 3, "Coffee maker (warming plate)"},
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

func TestDescribeForAI(t *testing.T) {
	c := Cluster{
		Key: "0.27", Signature: describe([]float64{0, 400}, []float64{0, 4}), Runs: 900, Days: 3,
		MedianOnSec: 3, ShortOnSec: 2, LongOnSec: 4, MedianGapSec: 16, EnergyWh: 1200,
	}
	c.Hours[7], c.Hours[8] = 400, 500
	got := describeForAI(c, 2)
	for _, want := range []string{"400 W step", "120 V · leg 2", "resistive", "every 16 s", "07:400 08:500", "1.20 kWh"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestAskAIWithoutKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	if _, err := NewServer(Config{DataDir: t.TempDir()}).askAI(t.Context(), "claude"); err == nil {
		t.Error("expected an error without an API key")
	}
}

func TestChatPrompt(t *testing.T) {
	s := NewServer(Config{DataDir: t.TempDir()})
	if _, err := s.chatPrompt(); err == nil {
		t.Error("expected an error with no signatures")
	}
	// Two days of the same 400 W run -> one cluster in the prompt.
	day := func(d int) string { return dayKey(time.Now().AddDate(0, 0, -d)) }
	for d := 0; d < 2; d++ {
		start := startOfDay(time.Now().AddDate(0, 0, -d)).UnixMilli() + 8*3_600_000
		end := start + 3000
		s.writeJSON("events/"+day(d)+".json", dayRuns{Runs: []Run{{Start: start, End: &end, DP: []float64{0, 400}, DQ: []float64{0, 4}}}}, 0o644)
	}
	s.settings.Household = "Drip coffee maker in the kitchen."
	p, err := s.chatPrompt()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Drip coffee maker in the kitchen.", "400 W step", "For each key"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestEmptyListsAreNotNull(t *testing.T) {
	s := NewServer(Config{DataDir: t.TempDir()})
	if a := s.activities(nil, nil, nowMs()); a == nil {
		t.Error("activities must be an empty list, not nil (the UI expects an array)")
	}
	if c := s.clusters(); c == nil {
		t.Error("clusters must be an empty list, not nil")
	}
}

func TestAppliancesWithSeveralSignatures(t *testing.T) {
	s := NewServer(Config{DataDir: t.TempDir()})
	// Old single-signature form is migrated.
	s.appliances = []Appliance{{ID: "x", Name: "Coffee maker", DP: []float64{0, 400}, DQ: []float64{0, 4}}}
	s.migrateAppliances()
	if len(s.appliances[0].Signatures) != 1 || s.appliances[0].DP != nil {
		t.Fatalf("migration: %+v", s.appliances[0])
	}
	// Naming three printer signatures with the same name (any case) makes one appliance.
	for _, dp := range [][]float64{{344, 0}, {969, 0}, {62, 0}} {
		s.addSignature("laser printer", Sig{DP: dp, DQ: []float64{0, 0}})
	}
	s.addSignature("Laser Printer", Sig{DP: []float64{960, 0}, DQ: []float64{0, 0}}) // same as 969 W: not added again
	if len(s.appliances) != 2 || len(s.appliances[1].Signatures) != 3 {
		t.Fatalf("appliances: %+v", s.appliances)
	}
	end := int64(5000)
	if a := s.applianceFor(Run{Start: 0, End: &end, DP: []float64{965, 0}}); a == nil || a.Name != "laser printer" {
		t.Errorf("969 W run should match the printer, got %+v", a)
	}
	// Runs of different signatures of one appliance form one activity.
	acts := s.activities([]Run{
		{Start: 0, End: ptr(int64(4000)), DP: []float64{344, 0}, DQ: []float64{0, 0}},
		{Start: 150_000, End: ptr(int64(156_000)), DP: []float64{969, 0}, DQ: []float64{0, 0}},
		{Start: 153_000, End: ptr(int64(195_000)), DP: []float64{62, 0}, DQ: []float64{0, 0}},
	}, nil, 200_000)
	if len(acts) != 1 || acts[0].Runs != 3 || acts[0].Appliance == nil {
		t.Errorf("activities: %+v", acts)
	}
}

func TestLabelAndMoveSignature(t *testing.T) {
	s := NewServer(Config{DataDir: t.TempDir()})
	p := s.addSignature("Laser printer", Sig{DP: []float64{344, 0}, DQ: []float64{0, 0}})
	s.addSignature("Laser printer", Sig{DP: []float64{969, 0}, DQ: []float64{0, 0}})
	s.appliances[0].Signatures[1].Label = "printing"

	acts := s.activities([]Run{{Start: 0, End: ptr(int64(6000)), DP: []float64{969, 0}, DQ: []float64{0, 0}}}, nil, 10_000)
	if sp := acts[0].Spans[0]; sp.Sig != 1 || sp.Label != "printing" || acts[0].ApplianceID == nil {
		t.Errorf("span %+v", sp)
	}
	// Move the warm-up signature to a new appliance; the printer keeps the other one.
	if _, err := s.moveSignature(p.ID, 0, "Space heater"); err != nil {
		t.Fatal(err)
	}
	if len(s.appliances) != 2 || len(s.appliances[0].Signatures) != 1 || s.appliances[1].Name != "Space heater" {
		t.Fatalf("after move: %+v", s.appliances)
	}
	// Moving the last signature away removes the appliance.
	if _, err := s.moveSignature(s.appliances[0].ID, 0, "Space heater"); err != nil {
		t.Fatal(err)
	}
	if len(s.appliances) != 1 || len(s.appliances[0].Signatures) != 2 {
		t.Fatalf("after second move: %+v", s.appliances)
	}
}
