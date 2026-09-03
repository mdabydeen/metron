package bench

// Widget is a thing with a label.
type Widget struct {
	Label string
}

// Describe returns the widget's label.
func Describe(w Widget) string {
	return w.Label
}
