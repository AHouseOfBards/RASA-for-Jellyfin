package ui

import (
	"syscall"
	"unsafe"
)

const (
	mbYesNo        = 0x00000004
	mbIconQuestion = 0x00000020
	idYes          = 6
)

// Ask puts a yes/no question where there is no terminal to ask it in.
//
// Release builds have no console, so a question printed to stdout is a question
// nobody is asked. This is the same MessageBoxW used for fatal errors, with two
// buttons instead of one.
//
// Defaults to no: it returns false on any failure, and the caller only ever
// asks before doing something it would rather not do by accident.
func Ask(title, body string) bool {
	t, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return false
	}
	b, err := syscall.UTF16PtrFromString(body)
	if err != nil {
		return false
	}
	r, _, _ := messageBoxW.Call(0,
		uintptr(unsafe.Pointer(b)),
		uintptr(unsafe.Pointer(t)),
		uintptr(mbYesNo|mbIconQuestion|mbSetForeground|mbTopMost),
	)
	return r == idYes
}
