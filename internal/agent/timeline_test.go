package agent

import (
	"math"
	"testing"
	"time"
)

func TestGrowthLinear(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var s []Sample
	for i := 0; i < 20; i++ {
		s = append(s, Sample{T: t0.Add(time.Duration(i) * 500 * time.Millisecond), WorkingSet: uint64(100e6 + i*5e6)})
	}
	g := Growth(s, 10*time.Second)
	if math.Abs(g-10e6) > 1 {
		t.Fatalf("growth = %v, want 10e6 B/s", g)
	}
}

func TestTimelineKeepsWindow(t *testing.T) {
	tl := Timeline{keep: time.Minute}
	t0 := time.Unix(0, 0)
	for i := 0; i < 180; i++ {
		tl.Add(Sample{T: t0.Add(time.Duration(i) * time.Second)})
	}
	s := tl.Samples()
	if len(s) != 61 || s[0].T != t0.Add(119*time.Second) {
		t.Fatalf("kept %d samples starting %v", len(s), s[0].T)
	}
}

func series(n int, f func(i int) float64) []Sample {
	t0 := time.Unix(1000, 0)
	var s []Sample
	for i := 0; i < n; i++ {
		s = append(s, Sample{T: t0.Add(time.Duration(i) * time.Second), WorkingSet: uint64(f(i))})
	}
	return s
}

func TestClassifyGrowth(t *testing.T) {
	cases := []struct {
		name string
		s    []Sample
		want Shape
	}{
		{"leak", series(300, func(i int) float64 { return 100e6 + float64(i)*1e6 + float64(i%3)*2e5 }), ShapeSteady},
		{"spike", series(300, func(i int) float64 {
			if i < 295 {
				return 200e6 + float64(i%5)*1e5
			}
			return 200e6 + float64(i-294)*80e6
		}), ShapeSpike},
		{"flat", series(300, func(i int) float64 { return 500e6 + float64(i%7)*1e5 }), ShapeUnknown},
		{"too short", series(20, func(i int) float64 { return 100e6 + float64(i)*10e6 }), ShapeUnknown},
		{"sawtooth", series(300, func(i int) float64 { return 100e6 + float64(i%60)*5e6 }), ShapeUnknown},
	}
	for _, c := range cases {
		if got, _ := ClassifyGrowth(c.s); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTimelineGrowthMatchesWindow(t *testing.T) {
	tl := Timeline{keep: time.Hour}
	// Flat for 5 minutes, then +1 MB/s for the last 10 seconds.
	s := series(320, func(i int) float64 {
		if i < 300 {
			return 100e6
		}
		return 100e6 + float64(i-300)*1e6
	})
	for _, x := range s {
		tl.Add(x)
	}
	if g := tl.Growth(10 * time.Second); math.Abs(g-1e6) > 1 {
		t.Fatalf("10s growth = %v, want 1e6", g)
	}
	if g, all := tl.Growth(10*time.Second), Growth(s, 10*time.Second); g != all {
		t.Fatalf("Timeline.Growth %v != Growth %v", g, all)
	}
}
