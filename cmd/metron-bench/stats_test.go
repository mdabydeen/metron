package main

import "testing"

func TestMedian(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []int
		want int
	}{
		{"empty", nil, 0},
		{"one", []int{5}, 5},
		{"odd", []int{9, 1, 5}, 5},
		{"even", []int{1, 2, 3, 10}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := median(tc.in); got != tc.want {
				t.Fatalf("median(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestMedianDoesNotReorderItsInput(t *testing.T) {
	in := []int{3, 1, 2}
	median(in)
	percentile(in, 0.5)
	if in[0] != 3 {
		t.Fatalf("input was sorted in place: %v", in)
	}
}

func TestPercentile(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []int
		p    float64
		want int
	}{
		{"empty", nil, 0.95, 0},
		{"p95 of three", []int{10, 20, 30}, 0.95, 30},
		{"p50", []int{10, 20, 30}, 0.5, 20},
		{"zero clamps to first", []int{10, 20, 30}, 0, 10},
		{"over one clamps to last", []int{10, 20, 30}, 1.5, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := percentile(tc.in, tc.p); got != tc.want {
				t.Fatalf("percentile(%v, %v) = %d, want %d", tc.in, tc.p, got, tc.want)
			}
		})
	}
}

func TestRate(t *testing.T) {
	if got := rate(0, 0); got != 0 {
		t.Fatalf("rate(0,0) = %v, want 0", got)
	}
	if got := rate(1, 4); got != 0.25 {
		t.Fatalf("rate(1,4) = %v, want 0.25", got)
	}
}
