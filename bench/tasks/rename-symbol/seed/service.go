package bench

// Welcome builds the banner shown to a named visitor.
func Welcome(name string) string {
	return Greet() + ", " + name
}
