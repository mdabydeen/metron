package bench

import "strconv"

func itoa(n int) string {
	return strconv.Itoa(n)
}

// Report formats a total that has already been computed.
func Report(total int) string {
	return summarize(total)
}

// RunPipeline sums the inputs and formats the result.
func RunPipeline(xs []int) string {
	return Report(compute(xs))
}
