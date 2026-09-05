// Package proc starts helper programs without putting a window on screen.
//
// RASA shells out to a dozen small utilities to learn things Go cannot ask the
// operating system directly: the default gateway, the ARP table, which process
// holds a port, whether an address is leased. On Windows every one of those is
// a console program, and since 0.7 the wizard itself is built for the GUI
// subsystem so that double-clicking an installer leaves no black console
// window behind.
//
// The consequence was reported from a real run: pressing "Test again" threw two
// full-size blue PowerShell windows onto the screen, which flashed and
// vanished. The same pair appeared at startup. Nothing was wrong -- that is
// simply what starting a console program from a windowed one looks like -- but
// it reads as something going badly wrong, on the screen where the user is
// already unsure whether anything is working.
//
// Hidden is therefore not cosmetic and not optional. Anything RASA starts and
// reads the output of must go through it; see TestEverySubprocessIsHidden,
// which fails on a new exec.Command that does not.
package proc

import "os/exec"

// Run is Hidden(cmd).Run, for the common case.
func Run(cmd *exec.Cmd) error { return Hidden(cmd).Run() }

// Output is Hidden(cmd).Output.
func Output(cmd *exec.Cmd) ([]byte, error) { return Hidden(cmd).Output() }

// CombinedOutput is Hidden(cmd).CombinedOutput.
func CombinedOutput(cmd *exec.Cmd) ([]byte, error) { return Hidden(cmd).CombinedOutput() }
