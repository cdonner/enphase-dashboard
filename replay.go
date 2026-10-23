package main

// "enphase-dashboard replay FILE..." runs recorded raw readings (TSV from /api/debug/raw
// or data/raw/*.tsv.gz) through the appliance detector and prints the runs it finds.

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

func replay(files []string) error {
	d := NewDetector()
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		var rd io.Reader = f
		if strings.HasSuffix(name, ".gz") {
			if rd, err = gzip.NewReader(f); err != nil {
				return err
			}
		}
		sc := bufio.NewScanner(rd)
		for sc.Scan() {
			c := strings.Split(sc.Text(), "\t")
			if len(c) < 3 || (len(c)-1)%2 != 0 {
				continue
			}
			at, err := strconv.ParseInt(c[0], 10, 64)
			if err != nil {
				continue
			}
			legs := (len(c) - 1) / 2
			p, q := make([]float64, legs), make([]float64, legs)
			for i := 0; i < legs; i++ {
				p[i], _ = strconv.ParseFloat(c[1+i], 64)
				q[i], _ = strconv.ParseFloat(c[1+legs+i], 64)
			}
			d.Add(at, p, q)
		}
		f.Close()
	}
	show := func(r Run, end string) {
		sig := describe(r.DP, r.DQ)
		fmt.Printf("%s - %s  %6.0f W  %-16s %-9s dp=%v\n", time.UnixMilli(r.Start).Format("15:04:05"), end,
			sig.PowerW, sig.Legs, sig.Character, roundAll(r.DP))
	}
	for _, r := range d.drainClosed() {
		show(r, time.UnixMilli(*r.End).Format("15:04:05"))
	}
	for _, r := range d.open {
		show(r, "(still on)")
	}
	return nil
}

func roundAll(v []float64) []int {
	out := make([]int, len(v))
	for i, x := range v {
		out[i] = int(x + 0.5)
	}
	return out
}
