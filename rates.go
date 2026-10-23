package main

// Electricity rates for valuing EV charging sessions. Two modes:
//   - blended: one $/kWh for everything - entered by hand, or the average of the last
//     12 months of monthly rates
//   - monthly: a rate per month (most recent earlier month fills gaps)
//
// Monthly rates come from the settings (edited in the UI) and from an optional
// rates.csv in the data directory, which wins for months it covers. Each CSV line is
// "month,rate" or "month,kWh,amount" (from a utility bill); month is "2026-09" or "Sep-26".

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const rateFileName = "rates.csv"

type monthRate struct {
	Rate     float64
	Kwh, Amt float64 // from a bill line; 0 if the line only gave a rate
}

type rateFile struct {
	modTime time.Time
	months  map[string]monthRate
	err     error
}

// parseMonth accepts "2026-09" or "Sep-26" / "Sep-2026".
func parseMonth(s string) (string, error) {
	s = strings.Trim(strings.TrimSpace(s), `"`)
	for _, layout := range []string{"2006-01", "Jan-06", "Jan-2006", "Jan 06", "Jan 2006", "01/2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01"), nil
		}
	}
	return "", fmt.Errorf("unrecognized month %q", s)
}

func parseNumber(s string) (float64, error) {
	s = strings.NewReplacer("$", "", ",", "", `"`, "", " ", "").Replace(s)
	return strconv.ParseFloat(s, 64)
}

func parseRateFile(path string) (map[string]monthRate, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]monthRate{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sep := ","
		if strings.Contains(line, "\t") {
			sep = "\t" // pasted straight from a spreadsheet
		}
		cols := strings.Split(line, sep)
		month, err := parseMonth(cols[0])
		if err != nil {
			if n == 1 {
				continue // header row
			}
			return nil, fmt.Errorf("%s line %d: %v", rateFileName, n, err)
		}
		var m monthRate
		switch {
		case len(cols) >= 3:
			kwh, err1 := parseNumber(cols[1])
			amt, err2 := parseNumber(cols[2])
			if err1 != nil || err2 != nil || kwh <= 0 {
				return nil, fmt.Errorf("%s line %d: expected month,kWh,amount", rateFileName, n)
			}
			m = monthRate{Rate: amt / kwh, Kwh: kwh, Amt: amt}
		case len(cols) == 2:
			r, err := parseNumber(cols[1])
			if err != nil {
				return nil, fmt.Errorf("%s line %d: expected month,rate", rateFileName, n)
			}
			m = monthRate{Rate: r}
		default:
			return nil, fmt.Errorf("%s line %d: expected month,rate or month,kWh,amount", rateFileName, n)
		}
		if m.Rate < 0 || m.Rate > 5 {
			return nil, fmt.Errorf("%s line %d: rate %.4f $/kWh looks wrong", rateFileName, n, m.Rate)
		}
		out[month] = m
	}
	return out, sc.Err()
}

// rateFileMonths returns the rates from rates.csv, re-reading it when it changes.
// Caller holds s.mu.
func (s *Server) rateFileMonths() (map[string]monthRate, error) {
	path := filepath.Join(s.cfg.DataDir, rateFileName)
	st, err := os.Stat(path)
	if err != nil {
		s.rates = rateFile{}
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !st.ModTime().Equal(s.rates.modTime) {
		months, err := parseRateFile(path)
		s.rates = rateFile{modTime: st.ModTime(), months: months, err: err}
		if err != nil {
			s.logf("Rate file: %v", err)
		}
	}
	return s.rates.months, s.rates.err
}

// RateInfo describes the rates in effect; returned with the EV data for the UI.
type RateInfo struct {
	Mode          string   `json:"mode"` // "blended" or "monthly"
	BlendedRate   *float64 `json:"blendedRate"`
	BlendedSource string   `json:"blendedSource"` // how BlendedRate was set, for display
	AverageRate   *float64 `json:"averageRate"`   // last-12-months average, if there are monthly rates
	AverageFrom   string   `json:"averageFrom"`
	AverageTo     string   `json:"averageTo"`
	FileMonths    int      `json:"fileMonths"`
	FileError     *string  `json:"fileError"`
}

type rates struct {
	info    RateInfo
	monthly map[string]monthRate // settings + rates.csv
}

// currentRates merges the settings and the rate file. Caller holds s.mu.
func (s *Server) currentRates() rates {
	r := rates{monthly: map[string]monthRate{}, info: RateInfo{Mode: s.settings.Mode}}
	for m, v := range s.settings.Rates {
		r.monthly[m] = monthRate{Rate: v}
	}
	file, err := s.rateFileMonths()
	if err != nil {
		r.info.FileError = ptr(err.Error())
	}
	for m, v := range file {
		r.monthly[m] = v
	}
	r.info.FileMonths = len(file)

	// Average of the 12 most recent months up to now: total amount / total kWh when the
	// bill lines give both, otherwise the plain mean of the rates.
	var months []string
	now := time.Now().Format("2006-01")
	for m := range r.monthly {
		if m <= now {
			months = append(months, m)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(months)))
	if len(months) > 12 {
		months = months[:12]
	}
	if len(months) > 0 {
		var kwh, amt, sum float64
		weighted := true
		for _, m := range months {
			v := r.monthly[m]
			kwh, amt, sum = kwh+v.Kwh, amt+v.Amt, sum+v.Rate
			weighted = weighted && v.Kwh > 0
		}
		avg := sum / float64(len(months))
		if weighted {
			avg = amt / kwh
		}
		r.info.AverageRate = ptr(avg)
		r.info.AverageFrom, r.info.AverageTo = months[len(months)-1], months[0]
	}

	if r.info.Mode == "" {
		r.info.Mode = "blended"
		if len(s.settings.Rates) > 0 && s.settings.BlendedRate == nil {
			r.info.Mode = "monthly" // settings saved before the blended option existed
		}
	}
	switch {
	case s.settings.BlendedRate != nil:
		r.info.BlendedRate, r.info.BlendedSource = s.settings.BlendedRate, "entered"
	case r.info.AverageRate != nil:
		r.info.BlendedRate, r.info.BlendedSource = r.info.AverageRate, "average"
	}
	return r
}

// rateFor returns the $/kWh for a month ("YYYY-MM"), or nil if no rate is known.
func (r rates) rateFor(month string) *float64 {
	if r.info.Mode == "blended" {
		return r.info.BlendedRate
	}
	// Monthly: that month's rate, else the most recent earlier one, else the earliest.
	var best, earliest string
	for m := range r.monthly {
		if m <= month && m > best {
			best = m
		}
		if earliest == "" || m < earliest {
			earliest = m
		}
	}
	if best == "" {
		best = earliest
	}
	if best == "" {
		return nil
	}
	return ptr(r.monthly[best].Rate)
}
