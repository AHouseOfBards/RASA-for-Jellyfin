package ui

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

// Open asks the operating system to display a URL.
//
// This is the seam a native webview would replace. Until then the user's own
// browser renders the wizard, which is not what SPEC.md decision 4 pictured but
// is honest about what a zero-dependency build can do: a real webview needs
// WebView2 on Windows and WKWebView on macOS, both of which are cgo
// dependencies with their own installers.
func Open(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// rundll32 rather than "cmd /c start": start treats the first quoted
		// argument as a window title, and the URL carries an ampersand that
		// cmd would otherwise split the command line on.
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", url)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("opening a browser: %w", err)
	}
	// Not waited on: some handlers stay in the foreground for as long as the
	// browser is open, and blocking here would stall setup behind the window
	// it just opened.
	go func() { _ = cmd.Wait() }()
	return nil
}
