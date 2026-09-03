package bench

func compute(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	return total
}

// summarize formats a total. It does not call compute.
func summarize(n int) string {
	return "total=" + itoa(n)
}
