//go:build windows

package probe

import (
	"context"
	"os/exec"
	"time"
)

// arpTable returns the system ARP table as text.
func arpTable(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "arp", "-a").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
