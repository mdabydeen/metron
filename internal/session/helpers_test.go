package session

import "os"

func writeGarbage(path string) error {
	return os.WriteFile(path, []byte("not json"), 0o644)
}

func chmod(path string, mode os.FileMode) error {
	return os.Chmod(path, mode)
}

func isRoot() bool {
	return os.Geteuid() == 0
}
