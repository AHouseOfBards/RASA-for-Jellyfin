package wizard

import (
	"os"
	"path/filepath"
	"runtime"
)

// UninstallerPath is the OS uninstaller for RASA itself, or "" when there is
// none to run.
//
// Removing the setup app is not the same thing as removing remote access, and
// the difference is the whole design: the proxy, the scheduled task and
// Jellyfin's settings are installed to outlive RASA, so uninstalling leaves a
// working system. RemoveRemoteAccess is the other button, and it is the one
// that takes the system down.
//
// Only Windows has something to launch. A Linux install is a directory and a
// symlink, so the honest answer there is a command the user can read, which the
// done screen shows instead.
func (w *Wizard) UninstallerPath() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	// Beside the running binary, which is where the installer puts it. A
	// development build or an unzipped copy has no uninstaller and gets "".
	candidate := filepath.Join(filepath.Dir(exe), "uninstall.exe")
	if fi, err := os.Stat(candidate); err != nil || fi.IsDir() {
		return ""
	}
	return candidate
}
