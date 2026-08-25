//go:build !windows

package main

// ensureElevated does nothing away from Windows.
//
// There is no equivalent worth having. Re-running under sudo from a process
// that may have no controlling terminal is a good way to hang on a password
// prompt nobody can see, and the documented install (packaging/linux) puts
// rasa on PATH to be run as `sudo rasa`. A run without the rights fails at
// EnsureDirs with a message that says so.
func ensureElevated() (relaunched bool, err error) { return false, nil }
