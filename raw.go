package main

// Raw per-second recorder for diagnosing and tuning the appliance detector: the per-leg
// consumption (real and reactive power) fed to the detector, kept for 48 hours in memory
// and written per hour to data/raw/<date>/<hour>.tsv.gz, where it is kept for rawKeepDays.

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	rawMemory   = 48 * time.Hour
	rawKeepDays = 7
	rawMaxSpan  = 6 * time.Hour // longest range one API request may ask for
)

type rawSample struct {
	At   int64 // ms
	P, Q []float32
}

type rawRecorder struct {
	dir     string
	buf     []rawSample // ring, oldest first after wrap handling
	next    int
	full    bool
	hour    int64       // unix hour of `pending`
	pending []rawSample // samples of the current hour, written when it ends
	logf    func(string, ...any)
}

func newRawRecorder(dataDir string, logf func(string, ...any)) *rawRecorder {
	return &rawRecorder{dir: filepath.Join(dataDir, "raw"), buf: make([]rawSample, int(rawMemory/time.Second)), logf: logf}
}

// add records one reading. Caller holds the server lock.
func (r *rawRecorder) add(at int64, p, q []float64) {
	s := rawSample{At: at, P: make([]float32, len(p)), Q: make([]float32, len(q))}
	for i := range p {
		s.P[i], s.Q[i] = float32(p[i]), float32(q[i])
	}
	r.buf[r.next] = s
	r.next = (r.next + 1) % len(r.buf)
	r.full = r.full || r.next == 0

	hour := at / 3_600_000
	if r.hour != 0 && hour != r.hour && len(r.pending) > 0 {
		done := r.pending
		go r.writeHour(done) // keep file I/O off the sampling path
		r.pending = nil
	}
	r.hour = hour
	r.pending = append(r.pending, s)
}

func hourFile(dir string, hourStart time.Time) string {
	return filepath.Join(dir, hourStart.Format("2006-01-02"), hourStart.Format("15")+".tsv.gz")
}

func (r *rawRecorder) writeHour(samples []rawSample) {
	start := time.UnixMilli(samples[0].At).Truncate(time.Hour)
	path := hourFile(r.dir, start)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.logf("Raw recorder: %v", err)
		return
	}
	f, err := os.Create(path + ".tmp")
	if err != nil {
		r.logf("Raw recorder: %v", err)
		return
	}
	zw := gzip.NewWriter(f)
	w := bufio.NewWriter(zw)
	writeRaw(w, samples)
	w.Flush()
	zw.Close()
	f.Close()
	if err := os.Rename(path+".tmp", path); err != nil {
		r.logf("Raw recorder: %v", err)
	}
	r.prune()
}

// writeRaw writes samples as TSV: time (ms), then P per leg, then Q per leg.
func writeRaw(w io.Writer, samples []rawSample) {
	for _, s := range samples {
		var b strings.Builder
		b.WriteString(strconv.FormatInt(s.At, 10))
		for _, v := range s.P {
			b.WriteString("\t" + strconv.FormatFloat(float64(v), 'f', 1, 32))
		}
		for _, v := range s.Q {
			b.WriteString("\t" + strconv.FormatFloat(float64(v), 'f', 1, 32))
		}
		b.WriteString("\n")
		io.WriteString(w, b.String())
	}
}

func (r *rawRecorder) prune() {
	dirs, _ := os.ReadDir(r.dir)
	cutoff := time.Now().AddDate(0, 0, -rawKeepDays).Format("2006-01-02")
	for _, d := range dirs {
		if d.IsDir() && d.Name() < cutoff {
			os.RemoveAll(filepath.Join(r.dir, d.Name()))
		}
	}
}

// memory returns the in-memory samples in [from, to). Caller holds the server lock.
func (r *rawRecorder) memory(from, to int64) []rawSample {
	n := r.next
	if r.full {
		n = len(r.buf)
	}
	out := []rawSample{}
	for i := 0; i < n; i++ {
		idx := i
		if r.full {
			idx = (r.next + i) % len(r.buf)
		}
		if s := r.buf[idx]; s.At >= from && s.At < to {
			out = append(out, s)
		}
	}
	return out
}

// fromFiles reads samples in [from, to) from the hourly files.
func (r *rawRecorder) fromFiles(from, to int64) []rawSample {
	var out []rawSample
	for h := time.UnixMilli(from).Truncate(time.Hour); h.UnixMilli() < to; h = h.Add(time.Hour) {
		f, err := os.Open(hourFile(r.dir, h))
		if err != nil {
			continue
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			continue
		}
		sc := bufio.NewScanner(zr)
		for sc.Scan() {
			c := strings.Split(sc.Text(), "\t")
			at, _ := strconv.ParseInt(c[0], 10, 64)
			if at < from || at >= to || len(c) < 3 || (len(c)-1)%2 != 0 {
				continue
			}
			legs := (len(c) - 1) / 2
			s := rawSample{At: at, P: make([]float32, legs), Q: make([]float32, legs)}
			for i := 0; i < legs; i++ {
				p, _ := strconv.ParseFloat(c[1+i], 32)
				q, _ := strconv.ParseFloat(c[1+legs+i], 32)
				s.P[i], s.Q[i] = float32(p), float32(q)
			}
			out = append(out, s)
		}
		zr.Close()
		f.Close()
	}
	return out
}

// samples returns [from, to) from memory and files combined, without duplicates.
func (s *Server) rawSamples(from, to int64) ([]rawSample, error) {
	if to <= from || time.Duration(to-from)*time.Millisecond > rawMaxSpan {
		return nil, fmt.Errorf("range must be positive and at most %s", rawMaxSpan)
	}
	s.mu.Lock()
	mem := s.raw.memory(from, to)
	s.mu.Unlock()
	seen := map[int64]bool{}
	for _, x := range mem {
		seen[x.At] = true
	}
	all := mem
	for _, x := range s.raw.fromFiles(from, to) {
		if !seen[x.At] {
			all = append(all, x)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].At < all[j].At })
	return all, nil
}
