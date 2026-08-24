//go:build !windows && !linux

package service

import (
	"context"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// macOS is out of scope for v1 (SPEC.md decision 7): distributing outside the
// App Store requires notarization, which requires paid Apple Developer
// membership, and there is no signing budget. The interface is still satisfied
// so the rest of the code compiles and can report the limitation rather than
// failing to build.

type unsupportedManager struct{}

func newManager(_ *logging.Logger) (Manager, error) { return unsupportedManager{}, ErrUnsupported }

func (unsupportedManager) InstallService(context.Context, Definition) error { return ErrUnsupported }
func (unsupportedManager) StartService(context.Context, string) error       { return ErrUnsupported }
func (unsupportedManager) StopService(context.Context, string) error        { return ErrUnsupported }
func (unsupportedManager) RemoveService(context.Context, string) error      { return ErrUnsupported }
func (unsupportedManager) InstallTimer(context.Context, Timer) error        { return ErrUnsupported }
func (unsupportedManager) RemoveTimer(context.Context, string) error        { return ErrUnsupported }

func (unsupportedManager) ServiceStatus(context.Context, string) (Status, error) {
	return StatusUnknown, ErrUnsupported
}

func (unsupportedManager) TimerInstalled(context.Context, string) (bool, error) {
	return false, ErrUnsupported
}
