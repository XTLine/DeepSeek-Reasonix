package session

import (
	"path/filepath"

	"reasonix/internal/filelock"
)

func directoryOwnershipPath(dir string) string {
	return filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+".ownership.lock")
}

// Keep both claims for a writer's lifetime: the inner claim excludes existing
// writers and the outer claim remains usable while the directory is moved.
func acquireSessionWriter(dir string) (func(), error) {
	releaseDirectory, err := filelock.TryAcquire(directoryOwnershipPath(dir))
	if err != nil {
		return nil, err
	}
	releaseWriter, err := filelock.TryAcquire(filepath.Join(dir, "writer.lock"))
	if err != nil {
		releaseDirectory()
		return nil, err
	}
	return func() {
		releaseWriter()
		releaseDirectory()
	}, nil
}
