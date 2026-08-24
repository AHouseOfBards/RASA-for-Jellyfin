//go:build windows

package secrets

import (
	"errors"
	"syscall"
	"unsafe"
)

// DPAPI at machine scope. Machine scope rather than user scope is required
// because the reader is a service account — Caddy and the scheduled sync task —
// not the interactive user who ran RASA.
//
// crypt32 is reached through LazyDLL so the module needs no dependency on
// golang.org/x/sys.

var (
	crypt32            = syscall.NewLazyDLL("crypt32.dll")
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procCryptProtect   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotect = crypt32.NewProc("CryptUnprotectData")
	procLocalFree      = kernel32.NewProc("LocalFree")
)

const (
	cryptprotectUIForbidden  = 0x1
	cryptprotectLocalMachine = 0x4
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(b []byte) dataBlob {
	if len(b) == 0 {
		return dataBlob{}
	}
	return dataBlob{cbData: uint32(len(b)), pbData: &b[0]}
}

// bytes copies the blob's contents into Go memory before it is freed.
func (b dataBlob) bytes() []byte {
	if b.pbData == nil || b.cbData == 0 {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, unsafe.Slice(b.pbData, b.cbData))
	return out
}

func (b dataBlob) free() {
	if b.pbData != nil {
		procLocalFree.Call(uintptr(unsafe.Pointer(b.pbData)))
	}
}

type dpapiProtector struct{}

func newProtector() protector { return dpapiProtector{} }

func (dpapiProtector) Describe() string { return "DPAPI (machine scope)" }

func (dpapiProtector) Protect(plain []byte) ([]byte, error) {
	in := newBlob(plain)
	var out dataBlob
	r, _, err := procCryptProtect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // description
		0, // no additional entropy
		0, // reserved
		0, // no prompt struct
		uintptr(cryptprotectLocalMachine|cryptprotectUIForbidden),
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		if err == nil {
			err = errors.New("CryptProtectData failed")
		}
		return nil, err
	}
	defer out.free()
	return out.bytes(), nil
}

func (dpapiProtector) Unprotect(sealed []byte) ([]byte, error) {
	in := newBlob(sealed)
	var out dataBlob
	r, _, err := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // description out
		0, // no additional entropy
		0, // reserved
		0, // no prompt struct
		uintptr(cryptprotectUIForbidden),
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		if err == nil {
			err = errors.New("CryptUnprotectData failed")
		}
		return nil, err
	}
	defer out.free()
	return out.bytes(), nil
}
