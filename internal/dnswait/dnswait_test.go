package dnswait

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

// A tiny authoritative DNS server, so propagation can be tested without
// waiting on the real internet. It answers A and TXT queries from a value that
// the test can flip mid-run, which is the behaviour that matters: the record
// appears partway through polling.

type fakeDNS struct {
	addr    string
	conn    *net.UDPConn
	answerA atomic.Pointer[netip.Addr]
	txt     atomic.Pointer[string]
	queries atomic.Int32
}

func startDNS(t *testing.T) *fakeDNS {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("cannot bind a UDP port: %v", err)
	}
	f := &fakeDNS{conn: pc, addr: pc.LocalAddr().String()}
	t.Cleanup(func() { pc.Close() })

	go func() {
		buf := make([]byte, 512)
		for {
			n, src, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			f.queries.Add(1)
			resp := f.respond(buf[:n])
			if resp != nil {
				_, _ = pc.WriteToUDP(resp, src)
			}
		}
	}()
	return f
}

// respond builds a minimal DNS reply. Only the pieces Go's resolver needs are
// implemented: header flags, the echoed question, and one answer record.
func (f *fakeDNS) respond(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	// Find the end of the question name.
	i := 12
	for i < len(q) && q[i] != 0 {
		i += int(q[i]) + 1
	}
	if i+5 > len(q) {
		return nil
	}
	nameEnd := i + 1
	qtype := int(q[nameEnd])<<8 | int(q[nameEnd+1])
	questionEnd := nameEnd + 4

	out := make([]byte, 0, 512)
	out = append(out, q[0], q[1]) // id
	out = append(out, 0x84, 0x00) // response, authoritative
	out = append(out, q[4], q[5]) // qdcount
	answers := 0

	var rdata []byte
	switch qtype {
	case 1: // A
		if a := f.answerA.Load(); a != nil && a.Is4() {
			b := a.As4()
			rdata = b[:]
			answers = 1
		}
	case 16: // TXT
		if s := f.txt.Load(); s != nil {
			rdata = append([]byte{byte(len(*s))}, []byte(*s)...)
			answers = 1
		}
	}

	out = append(out, byte(answers>>8), byte(answers)) // ancount
	out = append(out, 0, 0, 0, 0)                      // ns, ar
	out = append(out, q[12:questionEnd]...)            // echo question

	if answers == 1 {
		out = append(out, 0xc0, 0x0c) // name pointer to offset 12
		out = append(out, byte(qtype>>8), byte(qtype))
		out = append(out, 0, 1)        // class IN
		out = append(out, 0, 0, 0, 30) // ttl
		out = append(out, byte(len(rdata)>>8), byte(len(rdata)))
		out = append(out, rdata...)
	}
	return out
}

func (f *fakeDNS) setA(a netip.Addr) { f.answerA.Store(&a) }
func (f *fakeDNS) setTXT(s string)   { f.txt.Store(&s) }

func newWaiter(t *testing.T, servers ...string) *Waiter {
	t.Helper()
	w := New(logging.Discard())
	w.Servers = servers
	w.Interval = 50 * time.Millisecond
	w.Timeout = 4 * time.Second
	return w
}

func TestWaitForAReturnsWhenRecordAppears(t *testing.T) {
	f := startDNS(t)
	want := netip.MustParseAddr("203.0.113.5")

	// The record shows up partway through polling — the real scenario.
	//
	// Gated on polling having actually started rather than on a sleep. A sleep
	// races the first query: under load it can finish first, the very first
	// lookup succeeds, and the assertion below then fails while nothing is
	// wrong with the code being tested.
	go func() {
		for f.queries.Load() < 2 {
			time.Sleep(5 * time.Millisecond)
		}
		f.setA(want)
	}()

	w := newWaiter(t, f.addr)
	if err := w.WaitForA(context.Background(), "mymedia.freeddns.org", want, nil); err != nil {
		t.Fatalf("should have seen the record appear: %v", err)
	}
	if f.queries.Load() < 2 {
		t.Errorf("expected repeated polling, saw %d queries", f.queries.Load())
	}
}

func TestWaitForATimesOutWhenRecordNeverAppears(t *testing.T) {
	f := startDNS(t)
	w := newWaiter(t, f.addr)
	w.Timeout = 600 * time.Millisecond

	err := w.WaitForA(context.Background(), "mymedia.freeddns.org",
		netip.MustParseAddr("203.0.113.5"), nil)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
	// The message must say how far it got, so a support log is actionable.
	if !strings.Contains(err.Error(), "of 1 nameservers") {
		t.Errorf("timeout should report progress: %v", err)
	}
}

func TestWaitForAIgnoresWrongAddress(t *testing.T) {
	// A stale record pointing somewhere else must not count as propagated,
	// or ACME would be invoked against an address that is not ours.
	f := startDNS(t)
	f.setA(netip.MustParseAddr("198.51.100.7"))

	w := newWaiter(t, f.addr)
	w.Timeout = 500 * time.Millisecond

	err := w.WaitForA(context.Background(), "mymedia.freeddns.org",
		netip.MustParseAddr("203.0.113.5"), nil)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("a different address must not satisfy the wait: %v", err)
	}
}

func TestWaitForTXT(t *testing.T) {
	f := startDNS(t)
	const challenge = "acme-challenge-value-123"
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.setTXT(challenge)
	}()

	w := newWaiter(t, f.addr)
	if err := w.WaitForTXT(context.Background(), "_acme-challenge.mymedia.freeddns.org", challenge, nil); err != nil {
		t.Fatalf("TXT wait failed: %v", err)
	}
}

func TestWaitForTXTIgnoresDifferentValue(t *testing.T) {
	f := startDNS(t)
	f.setTXT("some-other-token")

	w := newWaiter(t, f.addr)
	w.Timeout = 500 * time.Millisecond

	if err := w.WaitForTXT(context.Background(), "_acme-challenge.x.freeddns.org", "expected", nil); !errors.Is(err, ErrTimeout) {
		t.Fatalf("a different TXT value must not satisfy the wait: %v", err)
	}
}

func TestEveryServerMustAgree(t *testing.T) {
	// A record on one nameserver but not another is exactly what makes ACME
	// flaky, because the CA does not promise which it will ask.
	a := startDNS(t)
	b := startDNS(t)
	want := netip.MustParseAddr("203.0.113.5")
	a.setA(want) // only one of the two

	w := newWaiter(t, a.addr, b.addr)
	w.Timeout = 600 * time.Millisecond

	if err := w.WaitForA(context.Background(), "x.freeddns.org", want, nil); !errors.Is(err, ErrTimeout) {
		t.Fatalf("partial propagation must not count as done: %v", err)
	}

	// Once the second agrees, it completes.
	b.setA(want)
	w2 := newWaiter(t, a.addr, b.addr)
	if err := w2.WaitForA(context.Background(), "x.freeddns.org", want, nil); err != nil {
		t.Fatalf("full propagation should succeed: %v", err)
	}
}

func TestProgressIsReported(t *testing.T) {
	// The wizard needs this: a two-minute wait with no feedback reads as a
	// hang, and step 10 is where users force-quit.
	f := startDNS(t)
	want := netip.MustParseAddr("203.0.113.5")
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.setA(want)
	}()

	var calls int
	var last Progress
	w := newWaiter(t, f.addr)
	if err := w.WaitForA(context.Background(), "x.freeddns.org", want, func(p Progress) {
		calls++
		last = p
	}); err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("no progress reported")
	}
	if last.Servers != 1 || last.ServersOK != 1 {
		t.Errorf("final progress wrong: %+v", last)
	}
	if last.Elapsed == 0 {
		t.Error("elapsed time not reported")
	}
}

func TestCancellationStopsWaiting(t *testing.T) {
	f := startDNS(t)
	w := newWaiter(t, f.addr)
	w.Timeout = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := w.WaitForA(ctx, "x.freeddns.org", netip.MustParseAddr("203.0.113.5"), nil)
	if err == nil {
		t.Fatal("expected an error after cancellation")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("cancellation ignored, waited %v", time.Since(start))
	}
}

func TestNoNameserversIsAnError(t *testing.T) {
	// Falling back to the system resolver would reintroduce the negative
	// caching this package exists to avoid, so this must fail loudly.
	w := New(logging.Discard())
	w.Timeout = time.Second
	w.SystemResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return nil, errors.New("no dns")
		},
	}
	err := w.WaitForA(context.Background(), "nothing.invalid", netip.MustParseAddr("203.0.113.5"), nil)
	if err == nil {
		t.Fatal("expected an error when no nameservers can be found")
	}
	if errors.Is(err, ErrTimeout) {
		t.Errorf("should be a lookup failure, not a timeout: %v", err)
	}
}

func TestIPNetworkSelection(t *testing.T) {
	if ipNetwork(netip.MustParseAddr("203.0.113.5")) != "ip4" {
		t.Error("IPv4 should select ip4")
	}
	if ipNetwork(netip.MustParseAddr("2001:db8::1")) != "ip6" {
		t.Error("IPv6 should select ip6")
	}
}
