//go:build !windows

package probe

import (
	"context"
	"os/exec"
	"time"
)

// arpTable returns the system neighbour table as text.
//
// `ip neigh` is preferred because `arp` is deprecated and absent from many
// modern minimal distributions and containers.
func arpTable(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if out, err := exec.CommandContext(ctx, "ip", "neigh").Output(); err == nil && len(out) > 0 {
		return string(out), nil
	}
	out, err := exec.CommandContext(ctx, "arp", "-an").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
