package probe

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// Ports RASA may listen on. They live here rather than in the mode package
// because probing them is what happens first, and mode already depends on this
// package — defining them the other way round would be an import cycle.
//
// DNS-01 issuance removes any need for port 80, so a busy 443 is an
// inconvenience rather than an obstacle: Jellyfin clients accept a port in the
// server URL.
const (
	PortPreferred = 443
	PortFallback  = 8443
)

// ProbePorts reports which of the given ports are free on this machine.
//
// This is strictly about local availability. Whether a port is reachable from
// the internet cannot be determined from inside the network — that needs an
// external prober, which Phase 7 supplies.
func ProbePorts(ctx context.Context, log *logging.Logger, ports ...int) Ports {
	if log == nil {
		log = logging.Discard()
	}
	out := Ports{
		Free:   make(map[int]bool, len(ports)),
		Holder: make(map[int]string, len(ports)),
	}
	for _, p := range ports {
		free := portFree(p)
		out.Free[p] = free
		if !free {
			// Naming the holder turns an opaque failure into an obvious one
			// (SPEC.md §15). Best effort: an unidentified holder is still a
			// useful "something is using this".
			if h := identifyHolder(ctx, p); h != "" {
				out.Holder[p] = h
			}
		}
		log.Debug("port probe",
			slog.Int("port", p),
			slog.Bool("free", free),
			slog.String("holder", out.Holder[p]),
		)
	}
	return out
}

// portFree reports whether nothing local holds the port.
//
// Both the wildcard and the loopback address are tested, because a wildcard
// bind alone is not conclusive. On Linux a listener on 127.0.0.1:443 makes a
// subsequent wildcard bind fail, so one check would do — but on Windows the
// wildcard bind *succeeds* alongside it, and the port would be reported free
// while another process quietly keeps serving loopback traffic.
//
// Reporting a contended port as busy is the safe direction: the cost is
// falling back to 8443 unnecessarily, whereas the opposite is Caddy failing to
// start after setup claimed success.
func portFree(port int) bool {
	for _, host := range []string{"", "127.0.0.1"} {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
		if err != nil {
			return false
		}
		_ = ln.Close()
	}
	return true
}
