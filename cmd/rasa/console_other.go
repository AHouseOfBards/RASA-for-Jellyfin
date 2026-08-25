//go:build !windows

package main

// attachParentConsole does nothing away from Windows, where a process either
// has a terminal or does not and nothing has to be reattached.
func attachParentConsole() {}
