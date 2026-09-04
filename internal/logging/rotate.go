package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Log files are rotated because one of RASA's logs is written by something
// that runs forever.
//
// rasa.log is bounded by a setup run ending. sync.log is not: the address
// syncer wakes every ten minutes for the life of the machine. Its happy path
// is quiet — it only logs when the address actually moves — but its failure
// path writes an error every single run. A revoked credential is therefore
// silent, unnoticed, and appends roughly fifty thousand lines a year to a file
// nobody is watching. Caddy's own log already rolls; this is the equivalent
// for RASA's.
const (
	// DefaultMaxBytes is the size at which a log is rolled. Large enough to
	// hold many runs of context for a bug report, small enough to attach.
	DefaultMaxBytes int64 = 8 << 20
	// DefaultKeep is how many rolled files are retained alongside the live
	// one, so the bound on disk is (Keep+1) * MaxBytes.
	DefaultKeep = 2
)

// rotatingFile is an io.WriteCloser that rolls its file once it passes a size.
//
// Deliberately not a general logging library. It rolls on write, not on a
// timer, so a process that never exits is still bounded, and it holds the file
// open between writes because the syncer's whole run is shorter than the time
// an open would take.
type rotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int

	f *os.File
	n int64
}

func openRotating(path string, max int64, keep int) (*rotatingFile, error) {
	if max <= 0 {
		max = DefaultMaxBytes
	}
	if keep < 0 {
		keep = 0
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	r := &rotatingFile{path: path, max: max, keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	// Size is read rather than assumed zero: the common case is appending to a
	// log that is already most of the way to the limit.
	var size int64
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	r.f, r.n = f, size
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	// Rolled before the write, not after, so a record is never split across
	// two files. r.n > 0 keeps a single oversized record from rolling an
	// empty file forever.
	if r.n > 0 && r.n+int64(len(p)) > r.max {
		r.roll()
	}
	n, err := r.f.Write(p)
	r.n += int64(n)
	return n, err
}

// roll renames the live file out of the way and starts a new one.
//
// Every failure here degrades rather than propagating: losing the ability to
// log is not worth failing a sync over, and the caller has no useful response
// to "the log could not be rolled". What it must not do is give up on the size
// bound, which is why a rename that Windows refuses — because something else
// holds the file open — falls back to truncating in place.
func (r *rotatingFile) roll() {
	if err := r.f.Close(); err != nil {
		r.truncate()
		return
	}
	if r.keep == 0 {
		_ = os.Remove(r.path)
		if r.open() != nil {
			r.f, r.n = nil, 0
		}
		return
	}

	// Shift the existing backups down: .2 becomes .3, .1 becomes .2. The
	// oldest falls off the end.
	_ = os.Remove(r.backup(r.keep))
	for i := r.keep - 1; i >= 1; i-- {
		_ = os.Rename(r.backup(i), r.backup(i+1))
	}
	if err := os.Rename(r.path, r.backup(1)); err != nil {
		if r.open() == nil {
			r.truncate()
		} else {
			r.f, r.n = nil, 0
		}
		return
	}
	if r.open() != nil {
		r.f, r.n = nil, 0
	}
}

func (r *rotatingFile) backup(i int) string {
	return fmt.Sprintf("%s.%d", r.path, i)
}

// truncate is the last resort when the file cannot be renamed. It discards
// history to keep the promise that matters more: that a log left running for
// years does not fill the disk.
func (r *rotatingFile) truncate() {
	if r.f == nil {
		return
	}
	if err := r.f.Truncate(0); err != nil {
		return
	}
	if _, err := r.f.Seek(0, 0); err != nil {
		return
	}
	r.n = 0
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
