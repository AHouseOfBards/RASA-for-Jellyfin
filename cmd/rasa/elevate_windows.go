package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// RASA needs administrator rights for its whole run: installing a service,
// writing a firewall rule and registering a scheduled task all require them,
// and the listener port is not decided until the port step, so re-elevating
// mid-wizard is not an option (SPEC.md, The user's journey, step 3).
//
// packaging/windows/rasa.manifest says exactly that, and nothing embeds it in
// the binary -- there is no .syso and no resource step, so it has never had
// any effect. Setup worked anyway because the NSIS installer is elevated and
// Execs rasa.exe as a child, which inherits it. Every other way in does not:
// the Start Menu shortcut the installer creates, and "run RASA again and
// choose Remove remote access", which is the documented way to take remote
// access down. Those got an unprivileged process that failed partway through,
// after reporting progress.
//
// Elevating here rather than embedding the manifest keeps the build a plain
// `go build` with no resource compiler and no checked-in binary blob, and it
// covers `go run` during development too.

// tokenElevation is TokenElevation, from TOKEN_INFORMATION_CLASS.
const tokenElevation = 20

// errorCancelled is ERROR_CANCELLED, which is what declining UAC produces.
const errorCancelled = syscall.Errno(1223)

// elevationMarker is set on the relaunched process so that a relaunch which
// somehow arrives still unprivileged cannot become an endless chain of UAC
// prompts.
const elevationMarker = "RASA_ELEVATION_ATTEMPTED"

var (
	shell32       = syscall.NewLazyDLL("shell32.dll")
	shellExecuteW = shell32.NewProc("ShellExecuteW")
)

// ensureElevated relaunches this process with administrator rights if it does
// not already hold them. It reports whether the caller should now exit, having
// handed the work to the new process.
func ensureElevated() (relaunched bool, err error) {
	if isElevated() {
		return false, nil
	}
	if os.Getenv(elevationMarker) != "" {
		// The relaunch happened and this process is still unprivileged. Carry
		// on rather than prompting again, so the error the user finally sees
		// names the operation that actually needed the rights.
		return false, nil
	}
	if err := relaunchElevated(); err != nil {
		return false, err
	}
	return true, nil
}

// isElevated reports whether the process token carries administrator rights.
func isElevated() bool {
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return false
	}
	var token syscall.Token
	if err := syscall.OpenProcessToken(process, syscall.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()

	// TOKEN_ELEVATION is a single DWORD.
	var elevated, size uint32
	err = syscall.GetTokenInformation(token, tokenElevation,
		(*byte)(unsafe.Pointer(&elevated)), uint32(unsafe.Sizeof(elevated)), &size)
	return err == nil && elevated != 0
}

// relaunchElevated asks the shell to start this program again with the runas
// verb, which is what raises the UAC prompt.
func relaunchElevated() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this program: %w", err)
	}

	// Inherited by the child, which is how it knows it is the relaunch.
	if err := os.Setenv(elevationMarker, "1"); err != nil {
		return fmt.Errorf("preparing to restart with administrator rights: %w", err)
	}

	verb, err := syscall.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	file, err := syscall.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	// ShellExecuteW takes one string rather than an argument vector, and
	// EscapeArg renders each one the way CommandLineToArgvW reads it back.
	escaped := make([]string, 0, len(os.Args)-1)
	for _, a := range os.Args[1:] {
		escaped = append(escaped, syscall.EscapeArg(a))
	}
	params, err := syscall.UTF16PtrFromString(strings.Join(escaped, " "))
	if err != nil {
		return err
	}
	dir, err := syscall.UTF16PtrFromString(filepath.Dir(exe))
	if err != nil {
		return err
	}

	const swShowNormal = 1
	// Success is a value GREATER than 32; anything at or below is an error
	// code rather than a handle.
	r, _, callErr := shellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(params)),
		uintptr(unsafe.Pointer(dir)),
		swShowNormal,
	)
	if r > 32 {
		return nil
	}
	if errno, ok := callErr.(syscall.Errno); ok && errno == errorCancelled {
		return fmt.Errorf("RASA needs administrator rights to install the proxy service, " +
			"the firewall rule and the address updater. Run it again and choose Yes.")
	}
	return fmt.Errorf("could not restart with administrator rights: %w", callErr)
}
