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
