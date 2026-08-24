package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNotFound is returned by Load when no state file exists — a first run.
var ErrNotFound = errors.New("no state file")

// Store persists State to a single JSON file.
type Store struct{ path string }

// NewStore returns a Store backed by path.
func NewStore(path string) *Store { return &Store{path: path} }

// Path returns the file backing the store.
func (s *Store) Path() string { return s.path }

// Load reads the state file. It returns ErrNotFound if setup has never run,
// which is the signal for the wizard to start fresh rather than offer repair.
func (s *Store) Load() (*State, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		// A corrupt state file must not brick repair mode. Report it plainly
		// so the caller can offer a clean run instead.
		return nil, fmt.Errorf("state file is unreadable: %w", err)
	}
	if st.Version > CurrentVersion {
		return &st, fmt.Errorf("state written by a newer RASA (version %d)", st.Version)
	}
	return &st, nil
}

// Save writes the state atomically.
//
// Setup is interrupted often enough that a half-written state file is a real
// scenario: write to a temporary file in the same directory, then rename over
// the target, so a reader sees either the old state or the new one.
func (s *Store) Save(st *State) error {
	if st == nil {
		return errors.New("nil state")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Windows will not rename onto an existing file, so clear the way first.
	// The window this opens is covered by the temp file still being on disk.
	_ = os.Remove(s.path)
	return os.Rename(tmpName, s.path)
}

// Exists reports whether a state file is present.
func (s *Store) Exists() bool {
	_, err := os.Stat(s.path)
	return err == nil
}
