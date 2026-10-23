package main

// Built-in catalog of common household loads, used to suggest names for unnamed
// signatures. Ranges are typical North American values; a signature can match several
// entries, so suggestions are ranked by how well power and run time fit.

import (
	"math"
	"sort"
)

type hint struct {
	Name       string
	Circuit    string   // "240" (both legs), "120" (one leg), "240+" (both legs plus extra on one)
	Character  []string // acceptable characters
	MinW, MaxW float64
	MinS, MaxS float64 // typical on-time per run, seconds
}

var catalog = []hint{
	{"Electric water heater", "240", []string{"resistive"}, 3000, 5600, 300, 5400},
	{"Clothes dryer (heating element)", "240", []string{"resistive"}, 1800, 5800, 30, 1800},
	{"Clothes dryer", "240+", []string{"resistive", "mixed"}, 2000, 6500, 300, 4800},
	{"Oven / range element", "240", []string{"resistive"}, 1000, 5000, 20, 1200},
	{"Electric cooktop burner", "240", []string{"resistive"}, 1000, 3500, 10, 900},
	{"Sauna heater (element)", "240", []string{"resistive"}, 1500, 9000, 120, 7200},
	{"Hot tub / spa heater", "240", []string{"resistive"}, 1500, 6000, 600, 7200},
	{"Electric baseboard heat", "240", []string{"resistive"}, 500, 4000, 120, 3600},
	{"Electric furnace / heat strips", "240", []string{"resistive"}, 3000, 20000, 300, 3600},
	{"Air conditioner / heat pump", "240", []string{"motor", "mixed"}, 1200, 6000, 300, 7200},
	{"Well pump", "240", []string{"motor", "mixed"}, 600, 2500, 20, 600},
	{"Pool pump", "240", []string{"motor", "mixed"}, 400, 2500, 1800, 43200},
	{"Whirlpool tub / jacuzzi pump", "240", []string{"motor", "mixed"}, 700, 2200, 300, 3600},
	{"EV charger", "240", []string{"resistive", "mixed"}, 1400, 11500, 1800, 50000},
	{"Microwave", "120", []string{"mixed", "resistive"}, 700, 1700, 15, 900},
	{"Kettle / toaster", "120", []string{"resistive"}, 700, 1800, 30, 300},
	{"Hair dryer", "120", []string{"resistive", "mixed"}, 500, 1900, 30, 900},
	{"Space heater", "120", []string{"resistive"}, 500, 1600, 120, 7200},
	{"Dishwasher heating", "120", []string{"resistive"}, 600, 1500, 120, 2400},
	{"Clothes washer", "120", []string{"motor", "mixed"}, 200, 1000, 60, 1800},
	{"Whirlpool tub / jacuzzi pump", "120", []string{"motor", "mixed"}, 600, 1500, 300, 3600},
	{"Refrigerator / freezer", "120", []string{"motor", "mixed"}, 70, 250, 300, 3600},
	{"Furnace blower", "120", []string{"motor", "mixed"}, 150, 900, 180, 7200},
	{"Furnace igniter", "120", []string{"resistive"}, 250, 700, 10, 120},
	{"Sump pump", "120", []string{"motor", "mixed"}, 250, 1000, 5, 120},
	{"Dehumidifier", "120", []string{"motor", "mixed"}, 250, 750, 600, 14400},
	{"Coffee maker (brewing)", "120", []string{"resistive"}, 600, 1500, 60, 600},
	{"Coffee maker (warming plate)", "120", []string{"resistive"}, 250, 600, 1, 30},
	{"Espresso / coffee machine (keeping warm)", "120", []string{"resistive"}, 600, 1700, 2, 60},
	{"Laser printer (fuser warming)", "120", []string{"resistive", "mixed"}, 250, 1200, 1, 30},
	{"Laser printer (printing)", "120", []string{"resistive", "mixed"}, 400, 1500, 10, 600},
	{"Computer / monitors", "120", []string{"resistive", "mixed"}, 60, 450, 600, 43200},
	{"Iron / small heater", "120", []string{"resistive"}, 200, 600, 2, 60},
	{"Vacuum cleaner", "120", []string{"motor", "mixed"}, 400, 1500, 60, 1800},
}

func circuitOf(sig Signature) string {
	switch {
	case sig.Legs == "240 V":
		return "240"
	case len(sig.Legs) > 5 && sig.Legs[:5] == "240 V":
		return "240+"
	case len(sig.Legs) > 5 && sig.Legs[:5] == "120 V":
		return "120"
	}
	return ""
}

// suggest ranks catalog entries that fit a signature and typical on-time.
func suggest(sig Signature, onSec float64) []string {
	circuit := circuitOf(sig)
	type scored struct {
		name  string
		score float64
	}
	var hits []scored
	for _, h := range catalog {
		if h.Circuit != circuit || sig.PowerW < h.MinW*0.85 || sig.PowerW > h.MaxW*1.15 {
			continue
		}
		charOK := false
		for _, c := range h.Character {
			charOK = charOK || c == sig.Character
		}
		if !charOK {
			continue
		}
		// Closer to the middle of the range (log scale) scores higher; run time outside the
		// typical range is a penalty, not a disqualification.
		mid := math.Sqrt(h.MinW * h.MaxW)
		score := -math.Abs(math.Log(sig.PowerW / mid))
		if onSec < h.MinS {
			score -= math.Log(h.MinS / math.Max(onSec, 1))
		} else if onSec > h.MaxS {
			score -= math.Log(onSec / h.MaxS)
		}
		hits = append(hits, scored{h.Name, score})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	var out []string
	for i := 0; i < len(hits) && i < 3; i++ {
		out = append(out, hits[i].name)
	}
	return out
}
