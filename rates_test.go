package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseMonth(t *testing.T) {
	for in, want := range map[string]string{"2026-09": "2026-09", "Sep-26": "2026-09", "Sep-2026": "2026-09", `"Jan-25"`: "2025-01"} {
		if got, err := parseMonth(in); err != nil || got != want {
			t.Errorf("parseMonth(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := parseMonth("Month"); err == nil {
		t.Error("parseMonth(header) should fail")
	}
}

func writeRates(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rateFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRateFileAndModes(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(Config{DataDir: dir})
	// Bill lines (tab separated, as pasted from a spreadsheet) with a header row.
	writeRates(t, dir, "Month\tKWh\tBilled\nSep-25\t1000\t$300.00\nAug-25\t1000\t200.00\n# comment\n")

	r := s.currentRates()
	if r.info.FileError != nil {
		t.Fatalf("file error: %s", *r.info.FileError)
	}
	if r.info.FileMonths != 2 || r.info.Mode != "blended" || r.info.BlendedSource != "average" {
		t.Fatalf("info = %+v", r.info)
	}
	if got := *r.info.BlendedRate; math.Abs(got-0.25) > 1e-9 { // 500 / 2000 kWh
		t.Errorf("blended = %v, want 0.25", got)
	}

	// Monthly mode: exact month, gap filled by the most recent earlier month, earliest before the first.
	s.settings = Settings{Mode: "monthly", Rates: map[string]float64{"2025-08": 0.99}}
	r = s.currentRates()
	for month, want := range map[string]float64{"2025-09": 0.30, "2025-08": 0.20 /* file wins */, "2026-03": 0.30, "2024-01": 0.20} {
		if got := r.rateFor(month); got == nil || math.Abs(*got-want) > 1e-9 {
			t.Errorf("rateFor(%s) = %v, want %v", month, got, want)
		}
	}

	// An entered blended rate overrides the average.
	s.settings = Settings{Mode: "blended", BlendedRate: ptr(0.2)}
	if got := *s.currentRates().rateFor("2025-09"); got != 0.2 {
		t.Errorf("entered blended = %v", got)
	}

	// The file is re-read when it changes; errors are reported, not fatal.
	writeRates(t, dir, "2025-09,abc\n")
	future := time.Now().Add(time.Minute)
	os.Chtimes(filepath.Join(dir, rateFileName), future, future)
	if r := s.currentRates(); r.info.FileError == nil {
		t.Error("expected a file error for a bad line")
	}
}
