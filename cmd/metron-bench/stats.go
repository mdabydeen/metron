package main

import (
	"math"
	"slices"
)

// median returns the middle value of xs, or the mean of the two middle values
// when the count is even. An empty slice yields 0.
//
// The benchmark reports a median rather than a mean because a single run that
// blows the turn budget produces an enormous prompt-token count, and one such
// outlier in three repetitions would otherwise dominate the number the whole
// exercise exists to publish.
func median(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

// percentile returns the nearest-rank percentile of xs for p in [0,1]. An
// empty slice yields 0.
//
// Nearest-rank (rather than an interpolating definition) is deliberate: with
// the default three repetitions per cell there is nothing to interpolate, and
// a p95 that reports an actually-observed run is easier to argue about than
// one that reports a number nobody measured.
func percentile(xs []int, p float64) int {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	rank := int(math.Ceil(p*float64(len(s)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(s) {
		rank = len(s) - 1
	}
	return s[rank]
}

// rate returns hits/total as a fraction, and 0 when nothing ran.
func rate(hits, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}
