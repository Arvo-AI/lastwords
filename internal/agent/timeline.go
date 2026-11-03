package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/Arvo-AI/lastwords/internal/procfs"
)

// Sample is one memory reading.
type Sample struct {
	T          time.Time
	WorkingSet uint64
	Usage      uint64
}

// Timeline is a bounded history of samples.
type Timeline struct {
	samples []Sample
	keep    time.Duration
}

// Add appends a sample and drops ones older than keep.
func (tl *Timeline) Add(s Sample) {
	tl.samples = append(tl.samples, s)
	cut := 0
	for cut < len(tl.samples) && s.T.Sub(tl.samples[cut].T) > tl.keep {
		cut++
	}
	// Reslicing is enough: append moves the live tail to a new array when it
	// runs out of capacity, so the dropped head is freed without a copy per tick.
	tl.samples = tl.samples[cut:]
}

// Samples returns a copy of the history.
func (tl *Timeline) Samples() []Sample { return append([]Sample(nil), tl.samples...) }

// Len is the number of samples held.
func (tl *Timeline) Len() int { return len(tl.samples) }

// Growth is the working-set slope over the last window, without copying the
// history (this runs for every container on every tick).
func (tl *Timeline) Growth(window time.Duration) float64 {
	i := len(tl.samples)
	if i == 0 {
		return 0
	}
	end := tl.samples[i-1].T
	for i > 0 && end.Sub(tl.samples[i-1].T) <= window {
		i--
	}
	return Growth(tl.samples[i:], window)
}

// Growth returns the working-set slope in bytes/second over the last window,
// using least squares so a single noisy sample does not dominate.
func Growth(samples []Sample, window time.Duration) float64 {
	if len(samples) < 2 {
		return 0
	}
	end := samples[len(samples)-1].T
	var xs, ys []float64
	for _, s := range samples {
		if end.Sub(s.T) <= window {
			xs = append(xs, s.T.Sub(end).Seconds())
			ys = append(ys, float64(s.WorkingSet))
		}
	}
	n := float64(len(xs))
	if n < 2 {
		return 0
	}
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}

// Shape describes how memory got to where it is.
type Shape int

const (
	ShapeUnknown Shape = iota // too short or too noisy to say
	ShapeSteady               // a straight climb: leak or unbounded cache
	ShapeSpike                // most of the rise came at the very end
)

// ClassifyGrowth looks at the whole timeline. It needs at least a minute of
// samples and a rise of at least 10% to say anything. It also returns how
// long the final rise took (for spikes) or the whole climb (for steady).
func ClassifyGrowth(samples []Sample) (Shape, time.Duration) {
	if len(samples) < 10 {
		return ShapeUnknown, 0
	}
	first, last := samples[0], samples[len(samples)-1]
	span := last.T.Sub(first.T)
	if span < time.Minute || last.WorkingSet <= first.WorkingSet || float64(last.WorkingSet-first.WorkingSet) < 0.1*float64(last.WorkingSet) {
		return ShapeUnknown, 0
	}
	// When did memory first get halfway from where it started to where it ended?
	half := first.WorkingSet + (last.WorkingSet-first.WorkingSet)/2
	var t50 time.Time
	for _, s := range samples {
		if s.WorkingSet >= half {
			t50 = s.T
			break
		}
	}
	if tail := last.T.Sub(t50); tail <= span/10 {
		return ShapeSpike, tail
	}
	if r2(samples) >= 0.8 {
		return ShapeSteady, span
	}
	return ShapeUnknown, 0
}

// r2 is the coefficient of determination of a least-squares line through the
// working set over time: 1 means a perfectly straight climb.
func r2(samples []Sample) float64 {
	n := float64(len(samples))
	t0 := samples[0].T
	var sx, sy, sxx, sxy, syy float64
	for _, s := range samples {
		x, y := s.T.Sub(t0).Seconds(), float64(s.WorkingSet)
		sx, sy, sxx, sxy, syy = sx+x, sy+y, sxx+x*x, sxy+x*y, syy+y*y
	}
	cov := n*sxy - sx*sy
	vx, vy := n*sxx-sx*sx, n*syy-sy*sy
	if vx <= 0 || vy <= 0 {
		return 0
	}
	return cov * cov / (vx * vy)
}

// CSV renders samples as a CSV file.
func CSV(samples []Sample, limit uint64) string {
	var b strings.Builder
	b.WriteString("time,working_set_bytes,usage_bytes,limit_bytes,percent_of_limit\n")
	for _, s := range samples {
		pct := ""
		if limit > 0 {
			pct = fmt.Sprintf("%.1f", 100*float64(s.WorkingSet)/float64(limit))
		}
		fmt.Fprintf(&b, "%s,%d,%d,%d,%s\n", s.T.UTC().Format(time.RFC3339Nano), s.WorkingSet, s.Usage, limit, pct)
	}
	return b.String()
}

// Sparkline renders the working-set history as a one-line text chart, with
// the start and end values, for summaries read in a terminal or by an agent.
func Sparkline(samples []Sample, width int) string {
	if len(samples) < 2 {
		return ""
	}
	bars := []rune("▁▂▃▄▅▆▇█")
	// Downsample to width buckets, keeping the max of each bucket.
	if width > len(samples) {
		width = len(samples)
	}
	vals := make([]uint64, width)
	for i, s := range samples {
		j := i * width / len(samples)
		if s.WorkingSet > vals[j] {
			vals[j] = s.WorkingSet
		}
	}
	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	var b strings.Builder
	for _, v := range vals {
		k := 0
		if hi > lo {
			k = int(float64(v-lo) / float64(hi-lo) * float64(len(bars)-1))
		}
		b.WriteRune(bars[k])
	}
	span := samples[len(samples)-1].T.Sub(samples[0].T).Round(time.Second)
	return fmt.Sprintf("%s  %s → %s over %s", b.String(), procfs.Bytes(samples[0].WorkingSet), procfs.Bytes(samples[len(samples)-1].WorkingSet), span)
}
