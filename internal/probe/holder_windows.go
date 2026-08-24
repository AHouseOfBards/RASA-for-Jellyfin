//go:build windows

package probe

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// identifyHolder names the process listening on a port.
//
// Best effort by design: an unidentified holder still yields a useful "something
// is using this port", and setup must never fail because a diagnostic aid did.
// netstat is used rather than iphlpapi because RASA runs once and a second of
// subprocess cost is cheaper than maintaining a syscall binding for a message
// that is only ever advisory.
func identifyHolder(ctx context.Context, port int) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	pid := listeningPID(ctx, port)
	if pid == 0 {
		return ""
	}
	if name := processName(ctx, pid); name != "" {
		return friendlyProcessName(name)
	}
	return ""
}

// listeningPID finds the PID owning a LISTENING socket on the port.
func listeningPID(ctx context.Context, port int) int {
	out, err := exec.CommandContext(ctx, "netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return 0
	}
	suffix := ":" + strconv.Itoa(port)
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		// Proto  Local Address  Foreign Address  State  PID
		if len(f) < 5 || !strings.EqualFold(f[3], "LISTENING") {
			continue
		}
		local := f[1]
		// Match the port exactly, so 443 does not also match 4433 or 8443.
		if i := strings.LastIndex(local, ":"); i < 0 || local[i:] != suffix {
			continue
		}
		if pid, err := strconv.Atoi(f[4]); err == nil {
			return pid
		}
	}
	return 0
}

func processName(ctx context.Context, pid int) string {
	out, err := exec.CommandContext(ctx, "tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH", "/FO", "CSV").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if line == "" || strings.HasPrefix(line, "INFO:") {
		return ""
	}
	// "name.exe","1234","Services","0","1,234 K"
	if i := strings.Index(line, `","`); i > 1 {
		return strings.Trim(line[:i], `"`)
	}
	return ""
}

// friendlyProcessName turns an executable name into something a user might
// recognise. An unmapped name is returned as-is, which is still better than
// nothing.
func friendlyProcessName(exe string) string {
	switch strings.ToLower(exe) {
	case "system", "system.exe":
		// Port 443 held by "System" almost always means http.sys, which IIS,
		// WinRM, BranchCache and Hyper-V all sit behind.
		return "a built-in Windows service"
	case "w3wp.exe", "inetinfo.exe", "iisexpress.exe":
		return "IIS"
	case "httpd.exe":
		return "Apache"
	case "nginx.exe":
		return "nginx"
	case "caddy.exe":
		return "Caddy"
	case "com.docker.backend.exe", "dockerd.exe", "docker.exe":
		return "Docker"
	case "vmware-hostd.exe":
		return "VMware"
	case "jellyfin.exe":
		return "Jellyfin"
	case "plex media server.exe":
		return "Plex"
	}
	return strings.TrimSuffix(exe, ".exe")
}
