package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const sampleKey = "dynu-api-key-abcdef1234567890"

func newTestStore(t *testing.T) *FileStore {
	t.Helper()
	return NewFileStore(filepath.Join(t.TempDir(), "credentials.dat"))
}

func TestSetGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set(DynuAPIKey, sampleKey); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(DynuAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	if got != sampleKey {
		t.Fatalf("got %q want %q", got, sampleKey)
	}
}

func TestGetMissingReturnsErrNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Get(DynuAPIKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSetReplacesPreviousValue(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set(DynuAPIKey, "old-value-aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(DynuAPIKey, sampleKey); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(DynuAPIKey)
	if got != sampleKey {
		t.Fatalf("value not replaced: %q", got)
	}
}

func TestDeleteRemovesValueAndIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set(DynuAPIKey, sampleKey); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(DynuAPIKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(DynuAPIKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("value survived delete: %v", err)
	}
	// Deleting an absent name must succeed — uninstall reruns are expected.
	if err := s.Delete(DynuAPIKey); err != nil {
		t.Fatalf("second delete failed: %v", err)
	}
}

func TestSecretIsNotStoredInPlaintextOnWindows(t *testing.T) {
	// On Windows DPAPI must actually transform the value. On Linux the
	// protection is file mode instead, so the check is skipped there — see
	// TestFilePermissionsAreRestrictive.
	if newProtector().Describe() == "file permissions (0600, root-owned)" {
		t.Skip("passthrough protector: protection is file mode, tested separately")
	}
	s := newTestStore(t)
	if err := s.Set(DynuAPIKey, sampleKey); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), sampleKey) {
		t.Fatal("secret is readable in the credential file")
	}
}

func TestFilePermissionsAreRestrictive(t *testing.T) {
	if runtime.GOOS == "windows" {
		// POSIX modes are inert on Windows; DPAPI carries the protection
		// there, which TestSecretIsNotStoredInPlaintextOnWindows covers.
		t.Skip("POSIX modes are inert on Windows")
	}
	s := newTestStore(t)
	if err := s.Set(DynuAPIKey, sampleKey); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("credential file is group/world accessible: %o", mode)
	}
}

func TestNamesListsWithoutRevealingValues(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set(DynuAPIKey, sampleKey); err != nil {
		t.Fatal(err)
	}
	names, err := s.Names()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != DynuAPIKey {
		t.Fatalf("unexpected names: %v", names)
	}
	for _, n := range names {
		if strings.Contains(n, sampleKey) {
			t.Fatal("Names leaked a value")
		}
	}
}

func TestEmptyNameRejected(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set("", "value"); err == nil {
		t.Fatal("expected empty name to be rejected")
	}
}

func TestNoTempFilesLeftBehind(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(filepath.Join(dir, "credentials.dat"))
	for i := 0; i < 3; i++ {
		if err := s.Set(DynuAPIKey, sampleKey); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestCorruptStoreReportsPlainly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.dat")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(path).Get(DynuAPIKey); err == nil {
		t.Fatal("expected an error for a corrupt store")
	} else if !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestEnvStoreReadsEnvironment(t *testing.T) {
	t.Setenv("RASA_DYNU_API_KEY", sampleKey)
	s := NewEnvStore("RASA_")

	got, err := s.Get(DynuAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	if got != sampleKey {
		t.Fatalf("got %q", got)
	}
	names, _ := s.Names()
	if len(names) != 1 {
		t.Fatalf("expected the key to be listed, got %v", names)
	}
}

func TestEnvStoreIsReadOnly(t *testing.T) {
	s := NewEnvStore("RASA_")
	if err := s.Set(DynuAPIKey, sampleKey); err == nil {
		t.Fatal("EnvStore.Set should refuse")
	}
	if err := s.Delete(DynuAPIKey); err == nil {
		t.Fatal("EnvStore.Delete should refuse")
	}
}

func TestEnvStoreMissingReturnsErrNotFound(t *testing.T) {
	s := NewEnvStore("RASA_NOTHING_")
	if _, err := s.Get(DynuAPIKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// Both implementations must satisfy Store.
var (
	_ Store = (*FileStore)(nil)
	_ Store = (*EnvStore)(nil)
)
