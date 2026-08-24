// Package secrets stores the credentials that must outlive RASA.
//
// SPEC.md §14: the Dynu API key cannot live in a RASA-owned keystore, because
// Caddy needs it to renew certificates and the scheduled sync task needs it to
// update addresses — both long after RASA has been uninstalled. So the secret
// belongs to the services that outlive RASA, in storage their service account
// can read and nothing else can:
//
//   - Windows: DPAPI at machine scope, under ProgramData.
//   - Linux:   a root-owned 0600 file, referenced by the systemd unit.
//   - Docker:  a mounted secret or environment variable (see EnvStore).
package secrets

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotFound is returned when a name has no stored value.
var ErrNotFound = errors.New("secret not found")

// Well-known secret names.
const (
	DynuAPIKey = "dynu_api_key"
)

// Store holds credentials.
type Store interface {
	Set(name, value string) error
	Get(name string) (string, error)
	Delete(name string) error
	Names() ([]string, error)
}

// protector encrypts values at rest. On Windows this is DPAPI; elsewhere it is
// a passthrough and file permissions carry the protection.
type protector interface {
	Protect([]byte) ([]byte, error)
	Unprotect([]byte) ([]byte, error)
	// Describe names the mechanism, for logging. It must never be used to
	// imply a value is protected when it is not.
	Describe() string
}

// FileStore persists secrets to a single file, encrypting each value with the
// platform protector.
type FileStore struct {
	path string
	p    protector
}

// NewFileStore returns a Store backed by path using the platform's protector.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path, p: newProtector()}
}

// Mechanism reports how values are protected at rest.
func (s *FileStore) Mechanism() string { return s.p.Describe() }

// Path returns the backing file.
func (s *FileStore) Path() string { return s.path }

type envelope map[string]string // name -> base64(protected value)

func (s *FileStore) load() (envelope, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return envelope{}, nil
	}
	if err != nil {
		return nil, err
	}
	var e envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("credential store is unreadable: %w", err)
	}
	if e == nil {
		e = envelope{}
	}
	return e, nil
}

func (s *FileStore) save(e envelope) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	// Write via a temp file so an interrupted save cannot destroy an existing
	// credential and strand the installed services.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".cred-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, os.ErrInvalid) {
		// Chmod is largely inert on Windows; DPAPI is the real protection
		// there, so a failure is not fatal.
		_ = err
	}
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
	_ = os.Remove(s.path)
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	return os.Chmod(s.path, 0o600)
}

// Set stores a value, replacing any previous one.
func (s *FileStore) Set(name, value string) error {
	if name == "" {
		return errors.New("empty secret name")
	}
	e, err := s.load()
	if err != nil {
		return err
	}
	sealed, err := s.p.Protect([]byte(value))
	if err != nil {
		return fmt.Errorf("protect %s: %w", name, err)
	}
	e[name] = base64.StdEncoding.EncodeToString(sealed)
	return s.save(e)
}

// Get returns a stored value, or ErrNotFound.
func (s *FileStore) Get(name string) (string, error) {
	e, err := s.load()
	if err != nil {
		return "", err
	}
	enc, ok := e[name]
	if !ok {
		return "", ErrNotFound
	}
	sealed, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("credential store is corrupt for %s: %w", name, err)
	}
	plain, err := s.p.Unprotect(sealed)
	if err != nil {
		// On Windows this is what a machine change or profile change looks
		// like. The caller should treat it as "ask the user again", not as a
		// crash.
		return "", fmt.Errorf("unprotect %s: %w", name, err)
	}
	return string(plain), nil
}

// Delete removes a value. Deleting an absent name succeeds.
func (s *FileStore) Delete(name string) error {
	e, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := e[name]; !ok {
		return nil
	}
	delete(e, name)
	return s.save(e)
}

// Names lists stored secret names. Values are never returned.
func (s *FileStore) Names() ([]string, error) {
	e, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(e))
	for k := range e {
		out = append(out, k)
	}
	return out, nil
}

// EnvStore reads secrets from the environment. This is the container path,
// where DPAPI and libsecret do not exist and the orchestrator supplies the
// secret (SPEC.md §17).
type EnvStore struct{ Prefix string }

// NewEnvStore returns a read-only Store over environment variables.
func NewEnvStore(prefix string) *EnvStore { return &EnvStore{Prefix: prefix} }

func (s *EnvStore) key(name string) string {
	return s.Prefix + strings.ToUpper(name)
}

// Set is unsupported: the orchestrator owns the environment.
func (s *EnvStore) Set(name, value string) error {
	return errors.New("environment secrets are read-only; set them in the compose file")
}

// Get reads the value from the environment.
func (s *EnvStore) Get(name string) (string, error) {
	if v, ok := os.LookupEnv(s.key(name)); ok && v != "" {
		return v, nil
	}
	return "", ErrNotFound
}

// Delete is unsupported.
func (s *EnvStore) Delete(name string) error {
	return errors.New("environment secrets are read-only")
}

// Names reports which known secrets are present.
func (s *EnvStore) Names() ([]string, error) {
	var out []string
	for _, n := range []string{DynuAPIKey} {
		if _, err := s.Get(n); err == nil {
			out = append(out, n)
		}
	}
	return out, nil
}
