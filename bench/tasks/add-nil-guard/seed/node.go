package bench

// Node is one link in a chain of text fragments.
type Node struct {
	Value string
	Next  *Node
}

// Length returns the number of bytes in the node's value.
func Length(n *Node) int {
	return len(n.Value)
}
