//go:build !windows

package secrets

// On Linux the protection is file ownership and mode, not encryption: the
// credential file is root-owned 0600 and referenced by the systemd unit's
// EnvironmentFile (SPEC.md §14). Encrypting at rest with a key that must also
// sit on the same disk would add ceremony without adding protection.
type passthroughProtector struct{}

func newProtector() protector { return passthroughProtector{} }

func (passthroughProtector) Describe() string { return "file permissions (0600, root-owned)" }

func (passthroughProtector) Protect(plain []byte) ([]byte, error) { return plain, nil }

func (passthroughProtector) Unprotect(sealed []byte) ([]byte, error) { return sealed, nil }
