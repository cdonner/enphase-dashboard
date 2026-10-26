package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTeach(t *testing.T) {
	s := NewServer(Config{DataDir: t.TempDir()})
	from := time.Now().Add(-10 * time.Minute).Truncate(time.Second).UnixMilli()
	// Two minutes before the window, then five in it: the owner switches a 1.2 kW microwave
	// on leg 2 on and off three times, while a 150 W fridge on leg 1 switches on once.
	for i := int64(-120); i < 300; i++ {
		p := []float64{300, 200}
		if i >= 20 && i < 270 && i%90 >= 20 && i%90 < 50 {
			p[1] += 1200
		}
		if i >= 100 {
			p[0] += 150
		}
		s.raw.add(from+i*1000, p, []float64{0, 0})
	}
	s.addSignature("Fridge", Sig{DP: []float64{150, 0}, DQ: []float64{0, 0}})
	got, err := s.teach(from, from+300_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates %+v", got)
	}
	if c := got[0]; c.Signature.Legs != "120 V · leg 2" || !near(c.Signature.PowerW, 1200, 30) || c.Ons != 3 || c.Offs != 3 || len(c.Times) != 6 {
		t.Errorf("first candidate %+v", c)
	}
	if c := got[1]; c.Ons != 1 || c.Offs != 0 || c.Appliance != "Fridge" {
		t.Errorf("second candidate %+v", c)
	}
	if _, err := s.teach(from, from+time.Hour.Milliseconds()); err == nil {
		t.Error("windows over 30 minutes should be refused")
	}
}

func TestCleanSlate(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(Config{DataDir: dir})
	s.addSignature("Kettle", Sig{DP: []float64{1500, 0}, DQ: []float64{0, 0}})
	s.saveAppliances()
	end := nowMs()
	s.recordRuns([]Run{{Start: end - 60_000, End: &end, DP: []float64{1500, 0}, DQ: []float64{0, 0}}})
	s.persistEvents()

	backup, err := s.cleanSlate()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.appliances) != 0 || len(s.loadDayRuns(dayKey(time.Now()))) != 0 {
		t.Errorf("left %d appliances, runs %+v", len(s.appliances), s.loadDayRuns(dayKey(time.Now())))
	}
	for _, f := range []string{appliancesFile, "events"} {
		if _, err := os.Stat(filepath.Join(dir, backup, f)); err != nil {
			t.Errorf("backup of %s: %v", f, err)
		}
	}
	// After a restart nothing comes back.
	if s2 := NewServer(Config{DataDir: dir}); len(s2.appliances) != 0 || len(s2.loadDayRuns(dayKey(time.Now()))) != 0 {
		t.Error("names or runs came back after a restart")
	}
	// New runs are recorded as usual.
	s.recordRuns([]Run{{Start: end, End: &end, DP: []float64{500, 0}, DQ: []float64{0, 0}}})
	if len(s.loadDayRuns(dayKey(time.Now()))) != 1 {
		t.Error("runs after the clean slate weren't recorded")
	}
}
