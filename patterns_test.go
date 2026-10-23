package main

import (
	"testing"
	"time"
)

func TestAnalyzePattern(t *testing.T) {
	day0 := time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local).UnixMilli()
	// Fridge-like: every 28 min, around the clock, for 2 days.
	var fridge []int64
	for t := day0; t < day0+2*86_400_000; t += 28 * 60_000 {
		fridge = append(fridge, t)
	}
	if p := analyzePattern(fridge, 2); p.Kind != "automatic" || !p.Regular || !near(p.PeriodSec, 1680, 1) {
		t.Errorf("fridge: %+v", p)
	}
	// Microwave-like: a few irregular uses, daytime only.
	var micro []int64
	for _, h := range []float64{7.2, 8.1, 12.4, 12.9, 18.3, 19.7} {
		micro = append(micro, day0+int64(h*3_600_000))
	}
	if p := analyzePattern(micro, 1); p.Kind != "operated" {
		t.Errorf("microwave: %+v", p)
	}
	if p := analyzePattern(micro[:2], 1); p.Kind != "" {
		t.Errorf("two runs should be too few: %+v", p)
	}
}

func TestRelationsAndGroups(t *testing.T) {
	// A furnace cycle: blower 485 W for 10 min; an inducer 82 W starts 2 s later and
	// stops before the blower; an igniter 300 W runs 30 s during the cycle. Plus an
	// unrelated heater at other times. Nothing is named.
	s := NewServer(Config{DataDir: t.TempDir()})
	base := startOfDay(time.Now()).UnixMilli() + 3_600_000
	var runs []Run
	for i := int64(0); i < 4; i++ {
		t0 := base + i*3_600_000
		runs = append(runs,
			Run{Start: t0, End: ptr(t0 + 600_000), DP: []float64{485, 0}, DQ: []float64{400, 0}},
			Run{Start: t0 + 2000, End: ptr(t0 + 540_000), DP: []float64{82, 0}, DQ: []float64{70, 0}},
			Run{Start: t0 + 60_000, End: ptr(t0 + 90_000), DP: []float64{300, 0}, DQ: []float64{0, 0}},
			Run{Start: t0 + 1_800_000, End: ptr(t0 + 1_821_000), DP: []float64{0, 1500}, DQ: []float64{0, 0}},
		)
	}
	s.writeJSON("events/"+dayKey(time.Now())+".json", dayRuns{Runs: runs}, 0o644)

	clusters, groups := s.clustersAndGroups()
	byPower := map[int]Cluster{}
	for _, c := range clusters {
		byPower[int(c.Signature.PowerW+0.5)] = c
	}
	igniter := byPower[300]
	found := false
	for _, r := range igniter.Related {
		if r.Label == "485 W motor" && r.How == "runs during" && r.Runs == 4 {
			found = true
		}
	}
	if !found {
		t.Errorf("igniter should run during the blower: %+v", igniter.Related)
	}
	if len(groups) != 1 || len(groups[0].Keys) != 3 {
		t.Fatalf("want one group of blower, inducer and igniter, got %+v", groups)
	}
	for _, k := range groups[0].Keys {
		if k == byPower[1500].Key {
			t.Error("the unrelated heater must not be in the group")
		}
	}
}

func TestNoGroupForChanceOverlap(t *testing.T) {
	// A 6 h EV charge once, and a fridge cycling every 28 min all day: the fridge runs
	// during the charge 13 times, but most of its runs don't, so no group.
	s := NewServer(Config{DataDir: t.TempDir()})
	day := startOfDay(time.Now()).UnixMilli()
	runs := []Run{{Start: day + 8*3_600_000, End: ptr(day + 14*3_600_000), DP: []float64{4600, 4600}, DQ: []float64{0, 0}}}
	for t0 := day; t0 < day+86_400_000-600_000; t0 += 28 * 60_000 {
		runs = append(runs, Run{Start: t0, End: ptr(t0 + 600_000), DP: []float64{120, 0}, DQ: []float64{100, 0}})
	}
	s.writeJSON("events/"+dayKey(time.Now())+".json", dayRuns{Runs: runs}, 0o644)
	if _, groups := s.clustersAndGroups(); len(groups) != 0 {
		t.Errorf("unexpected groups %+v", groups)
	}
}
