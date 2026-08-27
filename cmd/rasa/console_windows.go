package main

import (
	"os"
	"syscall"
)

// Release builds are linked with -H=windowsgui, which gives the process no
// console at all. That is the right default for an installer an ordinary
// person double-clicks: the alternative is a black window sitting behind the
// wizard for the whole run, looking like something they were not meant to see.
//
// It also takes away every printed line, including `rasa --version` and what
// `--diagnostics` reports, which matters to exactly the people who run this
// from a terminal.
//
// attachParentConsole gets that back. A GUI process started from a shell can
// attach to that shell's console and write there; started by double-click
// there is no parent console, the call fails, and nothing appears -- which is
// the outcome we wanted. One binary, both behaviours, decided by how it was
// started rather than by a flag anyone has to know about.
func attachParentConsole() bool {
	// Redirection wins, and must be checked first.
	//
	// `rasa --version > out.txt` hands this process a file handle, which Go
	// has already wired to os.Stdout, and attaching a console over the top of
	// that writes to the console instead and leaves the file empty. Measured:
	// exactly that, before this check existed.
	if usableStdout() {
		return true
	}

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	attachConsole := kernel32.NewProc("AttachConsole")

	// ATTACH_PARENT_PROCESS is (DWORD)-1.
	const attachParentProcess = ^uintptr(0)
	if r, _, _ := attachConsole.Call(attachParentProcess); r == 0 {
		return false // Started without a console. Nothing to write to, by design.
	}

	// CONOUT$ and CONIN$ always name the attached console.
	if out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = out
		os.Stderr = out
		return true
	}
	return false
}

// usableStdout reports whether this process was given somewhere to write.
func usableStdout() bool {
	h, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	return err == nil && h != 0 && h != syscall.InvalidHandle
}
