package ui

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

// launcher builds the command that hands a URL to the operating system.
//
// A variable rather than a function so a test can substitute something it can
// observe. Without that seam the only way to exercise Open is on a machine
// with a real browser, which is how a launcher that was being killed
// immediately after starting went unnoticed.
var launcher = func(ctx context.Context, url string) *exec.Cmd {
	switch runtime.GOOS {
	case "windows":
		// explorer.exe rather than rundll32 or "cmd /c start", because RASA
		// runs elevated. Explorer hands the URL to the logged-in user's
		// session, so the browser opens as that user; the other two launch it
		// as administrator, which browsers increasingly refuse outright and
		// which nobody should want anyway.
		//
		// It exits 1 on success, so its status is deliberately ignored. That
		// is safe here only because the caller prints the address regardless:
		// a launcher that cannot report failure must never be the only way to
		// reach the wizard.
		return exec.CommandContext(ctx, "explorer.exe", url)
	case "darwin":
		return exec.CommandContext(ctx, "open", url)
	default:
		return exec.CommandContext(ctx, "xdg-open", url)
	}
}

// Open asks the operating system to display a URL.
//
// This is the seam a native webview would replace. Until then the user's own
// browser renders the wizard, which is not what SPEC.md decision 4 pictured but
// is honest about what a build without a webview dependency can do: a real
// webview needs WebView2 on Windows and WKWebView on macOS, both of which bring
// their own runtimes and installers.
//
// A nil return does NOT mean a browser appeared. Handing a URL to the shell is
// fire-and-forget, and on an elevated process it frequently goes nowhere, so
// the caller must print the address whether this succeeds or not.
func Open(url string) error {
	// The timeout is a safety net against a launcher that hangs, and cancel is
	// deliberately NOT deferred here.
	//
	// exec.CommandContext kills the process when its context is done, so a
	// deferred cancel would fire the instant this function returns and kill
	// the launcher microseconds after starting it — which is exactly what
	// happened, silently, for both rundll32 and explorer.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)

	cmd := launcher(ctx, url)
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("opening a browser: %w", err)
	}
	// Not waited on synchronously: some handlers stay in the foreground for as
	// long as the browser is open, and blocking here would stall setup behind
	// the window it just opened. The reaper owns the context and releases it
	// once the launcher has actually exited.
	go func() {
		defer cancel()
		_ = cmd.Wait()
	}()
	return nil
}
