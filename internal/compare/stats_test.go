package compare

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Expected values come from sandbox script cp.py: an independent Python bisection on the binomial CDF.
func TestUpperBound95_KnownValues(t *testing.T) {
	cases := []struct {
		d, n int
		want float64
	}{
		{0, 1, 0.95},
		{0, 3, 0.6315968501359612},
		{0, 5, 0.4507197283469412},
		{0, 20, 0.13910834066826522},
		{0, 29, 0.09814462767729573},
		{0, 299, 0.009969146792899272},
		{1, 20, 0.2161061642068473},
		{2, 20, 0.2826185248858608},
		{5, 20, 0.455582404001749},
		{1, 5, 0.6574083180011387},
		{3, 10, 0.6066242161054134},
		{10, 100, 0.16371762327581363},
		{0, 100, 0.029513049607039932},
		{50, 100, 0.5863782853690875},
	}
	for _, c := range cases {
		got := UpperBound95(c.d, c.n)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("UpperBound95(%d,%d) = %.12f, want %.12f", c.d, c.n, got, c.want)
		}
	}
}

func TestUpperBound95_ZeroDisagreementClosedForm(t *testing.T) {
	for _, n := range []int{1, 2, 7, 20, 100, 1000} {
		want := 1 - math.Pow(0.05, 1/float64(n))
		if got := UpperBound95(0, n); math.Abs(got-want) > 1e-12 {
			t.Errorf("n=%d: %.15f vs closed form %.15f", n, got, want)
		}
		// "about 3/n": the rule of three is an approximation of the same number
		if n >= 20 {
			if r := UpperBound95(0, n) * float64(n); r < 2.7 || r > 3.1 {
				t.Errorf("n=%d: bound*n = %.3f, expected about 3", n, r)
			}
		}
	}
}

func TestUpperBound95_Edges(t *testing.T) {
	deadline := time.After(5 * time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if UpperBound95(0, 0) != 1 {
			t.Error("n = 0 supports no claim: the bound is 1")
		}
		if UpperBound95(5, 5) != 1 || UpperBound95(7, 5) != 1 {
			t.Error("d = n (or d > n) leaves the bound at 1")
		}
		if UpperBound95(-1, 10) != UpperBound95(0, 10) {
			t.Error("a negative d is treated as zero")
		}
		if v := UpperBound95(3000, 10000); v <= 0.29 || v >= 0.32 {
			t.Errorf("large n: %v", v)
		}
		_ = UpperBound95(0, math.MaxInt32)
	}()
	select {
	case <-done:
	case <-deadline:
		t.Fatal("UpperBound95 did not return: an unbounded loop at an edge")
	}
}

func TestUpperBound95_MonotoneAndBetweenZeroAndOne(t *testing.T) {
	prev := -1.0
	for d := 0; d <= 20; d++ {
		v := UpperBound95(d, 20)
		if v < prev || v < 0 || v > 1 {
			t.Fatalf("d=%d: %v after %v", d, v, prev)
		}
		prev = v
	}
	for n := 1; n < 60; n++ {
		if UpperBound95(0, n+1) >= UpperBound95(0, n) {
			t.Fatalf("more runs with no disagreement must tighten the bound (n=%d)", n)
		}
	}
}

func TestMinRuns(t *testing.T) {
	cases := map[float64]int{90: 29, 95: 59, 99: 299, 50: 5, 99.9: 2995, 100: 1}
	for p, want := range cases {
		if got := MinRuns(p); got != want {
			t.Errorf("MinRuns(%v) = %d, want %d", p, got, want)
		}
	}
	if MinRuns(0) != 1 || MinRuns(-5) != 1 {
		t.Error("a non-positive agreement needs a single run")
	}
}

func TestClaimSupported(t *testing.T) {
	// design 7.3: 5 runs cannot support 99%; that needs 299 runs with no disagreement
	if ClaimSupported(0, 5, 99) {
		t.Error("5 clean runs must not support 99%")
	}
	if ClaimSupported(0, 298, 99) {
		t.Error("298 clean runs must not support 99%")
	}
	if !ClaimSupported(0, 299, 99) {
		t.Error("299 clean runs support 99%")
	}
	if !ClaimSupported(0, 29, 90) || ClaimSupported(0, 28, 90) {
		t.Error("90% needs exactly 29 clean runs")
	}
	if ClaimSupported(1, 299, 99) {
		t.Error("one disagreement in 299 does not support 99%")
	}
	// DECISION: 100% is the plain statement 'every run made agreed'; it needs n >= 1 and d == 0
	if !ClaimSupported(0, 1, 100) || ClaimSupported(0, 0, 100) || ClaimSupported(1, 3, 100) {
		t.Error("a 100% rule is supported by at least one run and no disagreement")
	}
	if ClaimSupported(0, 0, 50) {
		t.Error("n = 0 supports nothing")
	}
}

func TestSupportsSentence(t *testing.T) {
	got := SupportsSentence(0, 20)
	want := "with 20 runs and no disagreement, the true disagreement rate is below 14% (95% confidence)"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	got = SupportsSentence(2, 20)
	if !strings.Contains(got, "2 of 20 runs disagreeing") || !strings.Contains(got, "below 29% (95% confidence)") {
		t.Errorf("got %q", got)
	}
	if got := UnsupportedSentence(5, 99); got != "5 runs cannot support 99%; that needs 299 runs with no disagreement" {
		t.Errorf("got %q", got)
	}
	if got := UnsupportedSentence(1, 90); got != "1 run cannot support 90%; that needs 29 runs with no disagreement" {
		t.Errorf("got %q", got)
	}
}
