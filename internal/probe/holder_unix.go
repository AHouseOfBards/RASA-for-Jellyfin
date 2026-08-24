//go:build !windows

package probe

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ssProcess matches the users:(("nginx",pid=123,fd=6)) field that ss emits.
var ssProcess = regexp.MustCompile(`users:\(\("([^"]+)"`)

// identifyHolder names the process listening on a port.
//
// Best effort by design. Process attribution generally requires privileges, so
// an unprivileged run may know the port is busy without knowing by what — that
// is still a useful thing to tell the user, and setup must never fail because
// a diagnostic aid did.
func identifyHolder(ctx context.Context, port int) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if name := viaSS(ctx, port); name != "" {
		return friendlyProcessName(name)
	}
	if name := viaLsof(ctx, port); name != "" {
		return friendlyProcessName(name)
	}
	return ""
}

func viaSS(ctx context.Context, port int) string {
	out, err := exec.CommandContext(ctx, "ss", "-lptnH", "sport = :"+strconv.Itoa(port)).Output()
	if err != nil {
		return ""
	}
	if m := ssProcess.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

func viaLsof(ctx context.Context, port int) string {
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fc").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "c") { // c<command>
			return strings.TrimPrefix(line, "c")
		}
	}
	return ""
}

// friendlyProcessName turns a process name into something a user might
// recognise. An unmapped name is returned as-is.
func friendlyProcessName(name string) string {
	switch strings.ToLower(name) {
	case "nginx":
		return "nginx"
	case "apache2", "httpd":
		return "Apache"
	case "caddy":
		return "Caddy"
	case "traefik":
		return "Traefik"
	case "docker-proxy", "dockerd", "containerd":
		return "Docker"
	case "jellyfin":
		return "Jellyfin"
	case "haproxy":
		return "HAProxy"
	case "lighttpd":
		return "lighttpd"
	}
	return name
}
