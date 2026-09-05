package proc

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW. It is the flag that actually works.
//
// HideWindow alone is not enough for a console program: it sets SW_HIDE in the
// startup info, which the console host is free to ignore, and in practice the
// window still appears for a moment. CREATE_NO_WINDOW tells Windows not to
// give the process a console at all. Both are set, because HideWindow is what
// covers a GUI program started the same way.
const createNoWindow = 0x08000000

// Hidden makes a command run without a console window, and returns it so it
// can be used inline.
func Hidden(cmd *exec.Cmd) *exec.Cmd {
	if cmd == nil {
		return cmd
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
	return cmd
}
