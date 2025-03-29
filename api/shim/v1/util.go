package v1

import (
	"path/filepath"

	"github.com/containerd/containerd/v2/pkg/atomicfile"
)

// WriteAddress writes a address file atomically
func WriteAddress(path, address string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	f, err := atomicfile.New(path, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write([]byte(address))
	if err != nil {
		f.Cancel()
		return err
	}
	return f.Close()
}
