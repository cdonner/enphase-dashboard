package main

// Pattern analysis over the run history, so signatures aren't judged one step at a time:
//
//   - relations: how often a signature starts together with another one (within
//     coStartWindow), or runs entirely during it - e.g. a furnace's inducer during its
//     blower run. Computed against named appliances and other unnamed signatures.
//   - groups: unnamed signatures that keep showing up together, probably parts of one
//     appliance, offered for naming in one go.
//   - patterns: whether a signature repeats regularly, runs around the clock, or only
//     when someone is around - "automatic" vs "operated".

import (
	"fmt"
	"math"
	"sort"
	"time"
)

type span struct{ start, end int64 }

// Relation: of this signature's runs, how many started together with / ran during `To`.
type Relation struct {
	To      string `json:"to"`      // appliance name, or cluster key for unnamed
	Named   bool   `json:"named"`   // To is a named appliance
	Label   string `json:"label"`   // display text for an unnamed partner, e.g. "82 W motor"
	Runs    int    `json:"runs"`    // matching runs of this signature
	Of      int    `json:"of"`      // all runs of this signature
	How     string `json:"how"`     // "starts with" or "runs during"
	Partner int    `json:"partner"` // runs of the partner (for unnamed ones)
}

type Pattern struct {
	Kind       string  `json:"kind"`       // "automatic", "operated" or "" (too few runs to tell)
	Text       string  `json:"text"`       // human-readable summary
	PeriodSec  float64 `json:"periodSec"`  // typical interval if it repeats regularly
	Regular    bool    `json:"regular"`    // intervals vary little
	NightShare float64 `json:"nightShare"` // share of starts between 22:00 and 06:00
}

// Group: unnamed signatures that probably belong to one appliance.
type Group struct {
	Keys  []string `json:"keys"`
	Runs  int      `json:"runs"`  // co-occurrences supporting the group (weakest link)
	Score float64  `json:"score"` // weakest link's share of runs
}

const (
	nestedSlack    = 5 * time.Second // a run "during" another may start/end this much outside it
	relationMinRun = 2               // relations need at least this many matching runs...
	relationShare  = 0.5             // ...covering at least this share of the signature's runs
	groupShare     = 0.6             // group members are related in both directions this strongly
)

func sortSpans(s []span) { sort.Slice(s, func(i, j int) bool { return s[i].start < s[j].start }) }

// countRelated counts runs of a that start within w of a run of b ("starts with"), and
// runs of a that lie within a run of b ("runs during"). b must be sorted by start.
func countRelated(a, b []span, w int64) (starts, during int) {
	slack := nestedSlack.Milliseconds()
	for _, r := range a {
		// starts with: any b start in [r.start-w, r.start+w]
		i := sort.Search(len(b), func(i int) bool { return b[i].start >= r.start-w })
		if i < len(b) && b[i].start <= r.start+w {
			starts++
			continue
		}
		// runs during: a b run that started before r (up to the slack) and ends after it.
		// Look back over b runs that started before r; long runs can start much earlier.
		j := sort.Search(len(b), func(j int) bool { return b[j].start > r.start+slack }) - 1
		for k := j; k >= 0 && k > j-50; k-- {
			if b[k].start <= r.start+slack && b[k].end >= r.end-slack && b[k].end-b[k].start > r.end-r.start {
				during++
				break
			}
		}
	}
	return
}

// covered counts runs of b that have at least one run of a starting together with them or
// lying within them. a must be sorted by start.
func covered(b, a []span, w int64) int {
	slack := nestedSlack.Milliseconds()
	n := 0
	for _, r := range b {
		i := sort.Search(len(a), func(i int) bool { return a[i].start >= r.start-w })
		for ; i < len(a) && a[i].start <= r.end+slack; i++ {
			if abs64(a[i].start-r.start) <= w || a[i].end <= r.end+slack {
				n++
				break
			}
		}
	}
	return n
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func relate(a, b []span) (n int, how string) {
	starts, during := countRelated(a, b, coStartWindow.Milliseconds())
	if starts >= during {
		return starts + during, "starts with"
	}
	return starts + during, "runs during"
}

// analyzePattern describes how a signature's runs repeat. starts must be sorted.
func analyzePattern(starts []int64, days int) Pattern {
	var p Pattern
	n := len(starts)
	if n == 0 {
		return p
	}
	night := 0
	for _, t := range starts {
		if h := time.UnixMilli(t).Hour(); h >= 22 || h < 6 {
			night++
		}
	}
	p.NightShare = float64(night) / float64(n)
	if n >= 4 {
		gaps := make([]float64, 0, n-1)
		for i := 1; i < n; i++ {
			gaps = append(gaps, float64(starts[i]-starts[i-1])/1000)
		}
		sort.Float64s(gaps)
		// Use the middle half so a missed run or an overnight pause doesn't spoil it.
		mid := gaps[len(gaps)/4 : len(gaps)-len(gaps)/4]
		var mean, sq float64
		for _, g := range mid {
			mean += g
		}
		mean /= float64(len(mid))
		for _, g := range mid {
			sq += (g - mean) * (g - mean)
		}
		cv := math.Sqrt(sq/float64(len(mid))) / mean
		p.PeriodSec = gaps[len(gaps)/2]
		p.Regular = cv < 0.35
	}
	switch {
	case n < 4:
		p.Text = "Too few runs yet to see a pattern."
	case p.Regular && p.NightShare >= 0.15:
		p.Kind, p.Text = "automatic", fmt.Sprintf("Repeats every %s, day and night: runs on its own (thermostat, compressor, pump)", fmtSeconds(p.PeriodSec))
	case p.Regular:
		p.Kind, p.Text = "automatic", fmt.Sprintf("Repeats every %s: probably runs on its own", fmtSeconds(p.PeriodSec))
	case p.NightShare >= 0.15:
		p.Kind, p.Text = "automatic", "Irregular, also at night: probably runs on its own (e.g. heating, cooling, pumps)"
	case night == 0 && n >= 5:
		p.Kind, p.Text = "operated", "Irregular and daytime only: probably switched on by someone"
	default:
		p.Text = "Irregular."
	}
	if days > 1 && n >= 4 {
		p.Text += fmt.Sprintf(" · about %.0f runs a day", float64(n)/float64(days))
	}
	return p
}

// analyze adds patterns, relations and groups to the clusters. spans[i] are the runs of
// out[i]; named maps appliance names to their runs.
func analyze(out []Cluster, spans [][]span, named map[string][]span, days int) []Group {
	for i := range spans {
		sortSpans(spans[i])
	}
	for _, s := range named {
		sortSpans(s)
	}
	names := make([]string, 0, len(named))
	for n := range named {
		names = append(names, n)
	}
	sort.Strings(names)

	// share[i][j]: share of out[i]'s runs related to out[j]'s runs;
	// cover[i][j]: share of out[j]'s runs that have a related run of out[i].
	share := make([][]float64, len(out))
	cover := make([][]float64, len(out))
	count := make([][]int, len(out))
	for i := range out {
		share[i], cover[i], count[i] = make([]float64, len(out)), make([]float64, len(out)), make([]int, len(out))
		starts := make([]int64, len(spans[i]))
		for k, s := range spans[i] {
			starts[k] = s.start
		}
		out[i].Pattern = analyzePattern(starts, days)
		out[i].Related = []Relation{}
		total := len(spans[i])
		for _, name := range names {
			n, how := relate(spans[i], named[name])
			if n >= relationMinRun || (total == 1 && n == 1) {
				if float64(n) >= relationShare*float64(total) {
					out[i].Related = append(out[i].Related, Relation{To: name, Named: true, Runs: n, Of: total, How: how})
				}
			}
		}
		for j := range out {
			if i == j {
				continue
			}
			n, how := relate(spans[i], spans[j])
			count[i][j], share[i][j] = n, float64(n)/float64(max(total, 1))
			cover[i][j] = float64(covered(spans[j], spans[i], coStartWindow.Milliseconds())) / float64(max(len(spans[j]), 1))
			if n >= relationMinRun && share[i][j] >= relationShare {
				sig := out[j].Signature
				out[i].Related = append(out[i].Related, Relation{
					To: out[j].Key, Label: fmt.Sprintf("%.0f W %s", sig.PowerW, sig.Character),
					Runs: n, Of: total, How: how, Partner: len(spans[j]),
				})
			}
		}
		sort.Slice(out[i].Related, func(a, b int) bool { return out[i].Related[a].Runs > out[i].Related[b].Runs })
	}

	// Groups: connected unnamed signatures where most runs of one are related to the other
	// and most runs of the other have such a run (covers nested parts like an igniter
	// during a blower run, but not a fridge that happens to cycle during a long EV charge).
	parent := make([]int, len(out))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	type link struct {
		runs  int
		score float64
	}
	weakest := map[int]link{}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			n := max(count[i][j], count[j][i])
			sc := math.Max(math.Min(share[i][j], cover[i][j]), math.Min(share[j][i], cover[j][i]))
			if n >= relationMinRun && sc >= groupShare {
				a, b := find(i), find(j)
				parent[a] = b
				l, ok := weakest[b]
				if la, oka := weakest[a]; oka && (!ok || la.score < l.score) {
					l, ok = la, true
				}
				if !ok || sc < l.score {
					l = link{n, sc}
				}
				weakest[b] = l
			}
		}
	}
	members := map[int][]string{}
	for i := range out {
		members[find(i)] = append(members[find(i)], out[i].Key)
	}
	groups := []Group{}
	for root, keys := range members {
		if len(keys) < 2 {
			continue
		}
		l := weakest[root]
		groups = append(groups, Group{Keys: keys, Runs: l.runs, Score: l.score})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Runs > groups[j].Runs })
	return groups
}
