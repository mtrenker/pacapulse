//go:build unix

package storage

import (
	"errors"
	"os"
	"syscall"
)

// lockFile is the exclusive advisory lock that makes this process the single
// owner of the database.
//
// A Kubernetes ReadWriteOnce volume is node-scoped and is not a process-level
// guarantee: two pods on the same node, or a debug pod mounting the volume, can
// both open the file. Every writing entry point therefore takes this lock
// before touching the database and holds it until the process exits.
//
// The lock is flock(2) on a dedicated file rather than on the database itself,
// so that it can be taken before SQLite opens anything, and released by the
// kernel if the process dies without a chance to clean up.
type lockFile struct{ file *os.File }

// acquireLock takes the lock without blocking. A lock held by another process
// is a startup failure, not something to wait for: waiting would turn a
// misconfigured second writer into a slow rollout instead of a loud refusal.
func acquireLock(path string) (*lockFile, error) {
	// #nosec G304 -- the path is application-owned: a fixed file name beneath
	// the configured data directory.
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, fileMode)
	if err != nil {
		return nil, wrap(ErrDataDirectory, "open lock file", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, wrap(ErrLocked, "acquire lock", err)
		}
		return nil, wrap(ErrDataDirectory, "acquire lock", err)
	}
	return &lockFile{file: file}, nil
}

// release drops the lock. Closing the descriptor would release it too; the
// explicit unlock keeps the release observable when a descriptor outlives the
// call for any reason.
func (l *lockFile) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil

	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	switch {
	case unlockErr != nil:
		return wrap(ErrDataDirectory, "release lock", unlockErr)
	case closeErr != nil:
		return wrap(ErrDataDirectory, "close lock file", closeErr)
	}
	return nil
}
