//go:build !windows

package proc

import "os/exec"

// Hidden does nothing off Windows: starting a program from another program
// does not create a window, so there is none to hide.
func Hidden(cmd *exec.Cmd) *exec.Cmd { return cmd }
