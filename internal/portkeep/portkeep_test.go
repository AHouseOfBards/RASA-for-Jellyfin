package portkeep

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/portmap"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/probe"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
)

// fakeMapper stands in for a router.
type fakeMapper struct {
	// existing is what Get reports, when found is true.
	existing portmap.Mapping
	found    bool
	getErr   error

	addErr   error
	granted  int // lease the router grants on Add
	addCalls int
	lastReq  portmap.Request
}

func (f *fakeMapper) Get(ctx context.Context, port int, proto string) (portmap.Mapping, bool, error) {
	return f.existing, f.found, f.getErr
}

func (f *fakeMapper) Add(ctx context.Context, req portmap.Request) (*portmap.Result, error) {
	f.addCalls++
	f.lastReq = req
	if f.addErr != nil {
		return nil, f.addErr
	}
	return &portmap.Result{
		Mapping: portmap.Mapping{
			ExternalPort:   req.ExternalPort,
			InternalPort:   req.InternalPort,
			InternalClient: req.InternalClient.String(),
			Protocol:       req.Protocol,
			LeaseSeconds:   f.granted,
			Enabled:        true,
		},
		VerifiedByReadback: true,
	}, nil
}

func target(client string) probe.MappingTarget {
	return probe.MappingTarget{
		Gateway:     netip.MustParseAddr("192.168.1.1"),
		LANAddress:  netip.MustParseAddr(client),
		ControlURL:  "http://192.168.1.1:5000/ctl",
		ServiceType: "urn:schemas-upnp-org:service:WANIPConnection:2",
	}
}

func keeperFor(m Mapper, t probe.MappingTarget, findErr error) *Keeper {
	return &Keeper{
		Log:  logging.Discard(),
		Find: func(context.Context) (probe.MappingTarget, error) { return t, findErr },
		NewMapper: func(string, string, *logging.Logger) Mapper {
			return m
		},
	}
}

func upnpState(port int) *state.State {
	st := state.NewState("test")
	st.PortMapping = &state.PortMapping{
		ExternalPort: port, InternalPort: port, Method: "upnp",
		LeaseSeconds: 604800, InternalClient: "192.168.1.50",
	}
	return st
}

// The whole point. A leased mapping is re-added, so it never reaches the end
// of its week.
func TestALeasedMappingIsRenewed(t *testing.T) {
	m := &fakeMapper{granted: 604800}
	st := upnpState(443)

	res := keeperFor(m, target("192.168.1.50"), nil).Run(context.Background(), st)

	if !res.Applicable || !res.OK || !res.Renewed {
		t.Fatalf("result = %+v, want an applied renewal", res)
	}
	if m.addCalls != 1 {
		t.Errorf("Add called %d times", m.addCalls)
	}
	if st.PortMapping.RenewedAt.IsZero() {
		t.Error("the renewal was not recorded, so a lapse would never become visible")
	}
	if st.PortMapping.LeaseSeconds != 604800 {
		t.Errorf("lease recorded as %d", st.PortMapping.LeaseSeconds)
	}
}

// DHCP moves this machine. A renewal that keeps pointing at the old address
// sends the user's traffic to whatever holds it now, which is worse than not
// renewing at all.
func TestRenewalFollowsThisMachineToANewAddress(t *testing.T) {
	m := &fakeMapper{granted: 604800}
	st := upnpState(443)
	st.PortMapping.InternalClient = "192.168.1.50"

	res := keeperFor(m, target("192.168.1.77"), nil).Run(context.Background(), st)

	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	if got := m.lastReq.InternalClient.String(); got != "192.168.1.77" {
		t.Errorf("renewed against %s, want the address this machine has now", got)
	}
	if st.PortMapping.InternalClient != "192.168.1.77" {
		t.Errorf("recorded client = %s", st.PortMapping.InternalClient)
	}
}

// A permanent mapping pointing at this machine needs nothing done to it.
// Rewriting a router's stored configuration every ten minutes for no reason is
// not free.
func TestAPermanentMappingIsLeftAlone(t *testing.T) {
	m := &fakeMapper{
		found: true,
		existing: portmap.Mapping{
			ExternalPort: 443, InternalPort: 443,
			InternalClient: "192.168.1.50", LeaseSeconds: 0, Enabled: true,
		},
	}
	st := upnpState(443)

	res := keeperFor(m, target("192.168.1.50"), nil).Run(context.Background(), st)

	if !res.OK {
		t.Fatalf("a permanent mapping was reported as a problem: %+v", res)
	}
	if res.Renewed {
		t.Error("rewrote a permanent mapping that was already correct")
	}
	if m.addCalls != 0 {
		t.Errorf("Add called %d times on a permanent mapping", m.addCalls)
	}
}

// The port is not RASA's to take back. Another device holding it is a real
// situation to report, not one to overwrite.
func TestAnotherDevicesMappingIsNotOverwritten(t *testing.T) {
	m := &fakeMapper{
		found: true,
		existing: portmap.Mapping{
			ExternalPort: 443, InternalClient: "192.168.1.167", LeaseSeconds: 0,
		},
	}
	st := upnpState(443)

	res := keeperFor(m, target("192.168.1.50"), nil).Run(context.Background(), st)

	if res.OK {
		t.Error("claimed the port was open when it points at another device")
	}
	if m.addCalls != 0 {
		t.Error("overwrote another device's port forward")
	}
	if res.Detail == "" || res.Err == nil {
		t.Errorf("the conflict was not reported: %+v", res)
	}
	// And it names the device, which is the one fact that makes it fixable.
	if want := "192.168.1.167"; !strings.Contains(res.Detail, want) {
		t.Errorf("detail does not name the device holding the port: %q", res.Detail)
	}
}

// A forward the user typed into their own router is theirs. RASA did not
// create it and must not touch it.
func TestAManualForwardIsNeverTouched(t *testing.T) {
	m := &fakeMapper{}
	st := state.NewState("test")
	st.PortMapping = &state.PortMapping{ExternalPort: 443, Method: "manual"}

	res := keeperFor(m, target("192.168.1.50"), nil).Run(context.Background(), st)

	if res.Applicable {
		t.Error("treated a hand-made forward as RASA's to renew")
	}
	if m.addCalls != 0 {
		t.Error("touched a hand-made forward")
	}
}

func TestNothingToDoWithoutAMapping(t *testing.T) {
	for _, st := range []*state.State{
		state.NewState("test"),
		func() *state.State {
			s := state.NewState("test")
			s.PortMapping = &state.PortMapping{Method: "upnp"} // no port
			return s
		}(),
	} {
		res := keeperFor(&fakeMapper{}, target("192.168.1.50"), nil).Run(context.Background(), st)
		if res.Applicable {
			t.Errorf("claimed there was a mapping to renew: %+v", res)
		}
	}
}

// Every one of these is a normal day for a home network, and none of them may
// produce a panic or a fatal error: the address sync and the certificate check
// run after this and matter more.
func TestEveryRouterFailureIsSurvived(t *testing.T) {
	cases := []struct {
		name    string
		mapper  *fakeMapper
		findErr error
	}{
		{"router gone", &fakeMapper{}, errors.New("no router replied")},
		{"add refused", &fakeMapper{addErr: &portmap.UPnPError{Code: 501}}, nil},
		{"port taken", &fakeMapper{addErr: &portmap.UPnPError{Code: 718}}, nil},
		{"read-back broken", &fakeMapper{getErr: errors.New("nope"), granted: 604800}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := upnpState(443)
			res := keeperFor(c.mapper, target("192.168.1.50"), c.findErr).Run(context.Background(), st)
			if !res.Applicable {
				t.Fatal("a router problem must still count as an applicable mapping")
			}
			if res.Detail == "" {
				t.Error("failed without saying anything a user could read")
			}
		})
	}
}

// A read-back that fails is not a reason to skip the renewal: the mapping may
// be gone, which is exactly when it most needs re-adding.
func TestARefusedReadBackStillRenews(t *testing.T) {
	m := &fakeMapper{getErr: errors.New("unsupported"), granted: 604800}
	st := upnpState(443)

	res := keeperFor(m, target("192.168.1.50"), nil).Run(context.Background(), st)
	if !res.OK || m.addCalls != 1 {
		t.Fatalf("result = %+v, adds = %d", res, m.addCalls)
	}
}

// A router that accepts the connection and then says nothing must not hold up
// the address sync queued behind it.
func TestARouterThatNeverAnswersIsBounded(t *testing.T) {
	k := &Keeper{
		Log:     logging.Discard(),
		Timeout: 150 * time.Millisecond,
		Find: func(ctx context.Context) (probe.MappingTarget, error) {
			<-ctx.Done()
			return probe.MappingTarget{}, ctx.Err()
		},
	}
	start := time.Now()
	res := k.Run(context.Background(), upnpState(443))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %s; the timeout is not being enforced", elapsed)
	}
	if res.OK {
		t.Error("claimed success against a router that never answered")
	}
}

// An unusable target is not the same as a found one, and must not reach the
// mapper.
func TestAnIncompleteTargetIsRefused(t *testing.T) {
	m := &fakeMapper{}
	bad := probe.MappingTarget{Gateway: netip.MustParseAddr("192.168.1.1")}
	res := keeperFor(m, bad, nil).Run(context.Background(), upnpState(443))
	if res.OK || m.addCalls != 0 {
		t.Errorf("used a target with no control URL: %+v", res)
	}
}

// SSDP is multicast UDP over a home network and a single round can simply be
// lost. A mapping confirmed twenty minutes ago has days of lease left, so one
// miss must not turn the health file red or fire an alert.
func TestOneMissedRoundIsNotAFault(t *testing.T) {
	now := time.Now()
	recent := Result{Applicable: true, OK: false, LastGood: now.Add(-20 * time.Minute)}
	if recent.Settled(now) {
		t.Error("a single miss twenty minutes after a good renewal was treated as a fault")
	}

	// But something that has been failing all day is not a lost datagram.
	stale := Result{Applicable: true, OK: false, LastGood: now.Add(-7 * time.Hour)}
	if !stale.Settled(now) {
		t.Error("seven hours of failures was still being excused")
	}

	// Never having worked is reportable immediately: there is no good run to
	// fall back on.
	never := Result{Applicable: true, OK: false}
	if !never.Settled(now) {
		t.Error("a mapping that has never been confirmed was excused")
	}

	// Success and non-applicable are always settled.
	for _, r := range []Result{
		{Applicable: true, OK: true},
		{Applicable: false},
	} {
		if !r.Settled(now) {
			t.Errorf("%+v was reported as unsettled", r)
		}
	}
}

// The grace window has to leave most of the lease in hand, or it is just a
// slower way of failing silently.
func TestTheGraceWindowIsShortNextToTheLease(t *testing.T) {
	const lease = 604800 * time.Second
	if GraceWindow >= lease/4 {
		t.Errorf("grace of %s against a lease of %s leaves too little margin", GraceWindow, lease)
	}
	// And long enough to cover a good number of ten-minute attempts.
	if GraceWindow < 2*time.Hour {
		t.Errorf("grace of %s is too short to ride out a flaky wireless network", GraceWindow)
	}
}
