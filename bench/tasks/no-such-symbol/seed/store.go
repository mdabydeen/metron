package bench

// Store holds widgets by label.
type Store struct {
	items map[string]Widget
}

// Put files a widget under its own label.
func (s *Store) Put(w Widget) {
	if s.items == nil {
		s.items = map[string]Widget{}
	}
	s.items[w.Label] = w
}
