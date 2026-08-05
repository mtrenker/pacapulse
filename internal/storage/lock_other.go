//go:build !unix

package storage

// lockFile is the unsupported-platform stand-in for the exclusive advisory
// lock. The service targets a Linux container; rather than start without the
// single-writer guarantee the data model depends on, it refuses to run.
type lockFile struct{}

func acquireLock(string) (*lockFile, error) {
	return nil, describe(ErrDataDirectory, "acquire lock",
		"exclusive advisory locking is not supported on this platform")
}

func (l *lockFile) release() error { return nil }
