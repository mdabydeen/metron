package bench

// Last returns the final element of xs, which must not be empty.
func Last(xs []int) int {
	return xs[len(xs)]
}
