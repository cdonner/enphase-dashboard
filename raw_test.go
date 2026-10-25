package main

import (
	"testing"
	"time"
)

func TestRawRecorder(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(Config{DataDir: dir})
	base := time.Date(2026, 10, 23, 10, 59, 0, 0, time.Local).UnixMilli()
	// Two minutes across an hour boundary; the first hour gets written to disk.
	for i := int64(0); i < 120; i++ {
		s.raw.add(base+i*1000, []float64{500 + float64(i), 300}, []float64{10, 20})
	}
	// Wait for the asynchronous hour file.
	path := hourFile(s.raw.dir, time.UnixMilli(base).Truncate(time.Hour))
	for i := 0; i < 50; i++ {
		if _, err := s.rawSamples(base, base+1000); err == nil {
			if got := s.raw.fromFiles(base, base+60_000); len(got) == 60 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.raw.fromFiles(base, base+60_000); len(got) != 60 || got[5].P[0] != 505 || got[5].Q[1] != 20 {
		t.Fatalf("file %s: %d samples, %+v", path, len(got), got)
	}
	all, err := s.rawSamples(base, base+120_000)
	if err != nil || len(all) != 120 || all[119].P[0] != 619 {
		t.Fatalf("combined: %d samples, err %v", len(all), err)
	}
	// After a restart only the files remain.
	s2 := NewServer(Config{DataDir: dir})
	if got, _ := s2.rawSamples(base, base+120_000); len(got) != 60 {
		t.Errorf("after restart: %d samples from files, want 60", len(got))
	}
	if _, err := s.rawSamples(base, base+7*3_600_000); err == nil {
		t.Error("ranges over 6 h should be refused")
	}
}

func TestRedetect(t *testing.T) {
	s := NewServer(Config{DataDir: t.TempDir()})
	y := time.Now().AddDate(0, 0, -1)
	date := dayKey(y)
	day := time.Date(y.Year(), y.Month(), y.Day(), 0, 0, 0, 0, time.Local)
	// Yesterday 20:00-21:00 recorded to disk: a 2 kW heater on 20:10-20:40.
	var samples []rawSample
	for t := day.Add(20 * time.Hour); t.Before(day.Add(21 * time.Hour)); t = t.Add(time.Second) {
		p := float32(300)
		if !t.Before(day.Add(20*time.Hour+10*time.Minute)) && t.Before(day.Add(20*time.Hour+40*time.Minute)) {
			p += 2000
		}
		samples = append(samples, rawSample{At: t.UnixMilli(), P: []float32{p, 200}, Q: []float32{0, 0}})
	}
	s.raw.writeHour(samples)
	// The stored day lost its runs (the bug this recovers from).
	s.writeJSON("events/"+date+".json", dayRuns{Runs: []Run{}}, 0o644)
	if len(s.loadDayRuns(date)) != 0 {
		t.Fatal("setup")
	}
	n, err := s.redetect(date)
	if err != nil || n != 1 {
		t.Fatalf("redetect: %d runs, %v", n, err)
	}
	runs := s.loadDayRuns(date)
	if len(runs) != 1 || !near(runs[0].DP[0], 2000, 30) || (*runs[0].End-runs[0].Start)/60000 != 30 {
		t.Errorf("runs %+v", runs)
	}
	if _, err := s.redetect(dayKey(time.Now().AddDate(0, 0, -30))); err == nil {
		t.Error("days without raw readings should be refused")
	}
}
