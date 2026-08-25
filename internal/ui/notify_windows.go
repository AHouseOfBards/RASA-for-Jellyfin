package ui

import (
	"syscall"
	"unsafe"
)

var (
	user32      = syscall.NewLazyDLL("user32.dll")
	messageBoxW = user32.NewProc("MessageBoxW")
)

const (
	mbIconInformation = 0x00000040
	mbSetForeground   = 0x00010000
	mbTopMost         = 0x00040000
)

// Notify shows the user a message with no console to print it to.
//
// Handing a URL to the shell is fire-and-forget: the launcher reports success
// as soon as it has started, which says nothing about whether a window ever
// appeared, and on an elevated process it has silently gone nowhere before.
// The printed address was the way out of that. Release builds are linked
// -H=windowsgui and have no console to print it to, so this is what replaces
// it -- without one, a browser that fails to appear leaves the user with no
// address, no window and no way forward at all.
//
// Blocking until dismissed, so callers that must not stall run it in a
// goroutine.
func Notify(title, body string) {
	t, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	b, err := syscall.UTF16PtrFromString(body)
	if err != nil {
		return
	}
	// Topmost and foreground: this appears precisely when the user is staring
	// at a desktop wondering whether anything happened, so it must not open
	// behind the window they are looking at.
	messageBoxW.Call(0,
		uintptr(unsafe.Pointer(b)),
		uintptr(unsafe.Pointer(t)),
		uintptr(mbIconInformation|mbSetForeground|mbTopMost),
	)
}
