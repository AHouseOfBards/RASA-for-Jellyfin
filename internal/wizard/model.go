// Package wizard drives the setup flow.
//
// SPEC.md §9 describes twelve steps, of which only three need the user at the
// keyboard. This package owns the sequencing, the branching, and the decision
// about what the user is shown; it owns no pixels. The UI subscribes to Model
// snapshots and posts intents back, which is what keeps the flow testable
// without a browser — and given how much of RASA can only be exercised against
// real hardware, the orchestration is the part that had better be tested.
//
// Every fork happens invisibly (SPEC.md §9). The user is told the outcome of a
// branch, never asked to choose one, so nothing in this package prompts.
package wizard

import (
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/domains"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/rasaerr"
	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/state"
)

// Screen is what the user is looking at.
type Screen string

const (
	// ScreenWelcome is journey step 4, and doubles as the repair screen when
	// a previous setup is found.
	ScreenWelcome Screen = "welcome"
	// ScreenChecking is journey step 5: four lines ticking green.
	ScreenChecking Screen = "checking"
	// ScreenBlocked is reached when pre-flight found something the user must
	// resolve before anything else can happen.
	ScreenBlocked Screen = "blocked"
	// ScreenJellyfin is journey step 6: sign in to the media server.
	ScreenJellyfin Screen = "jellyfin"
	// ScreenDynu is journey step 7: the embedded account signup and the paste
	// field beneath it.
	ScreenDynu Screen = "dynu"
	// ScreenName is journey step 8: pick a name.
	ScreenName Screen = "name"
	// ScreenPort is journey step 9. Usually passed through without stopping.
	ScreenPort Screen = "port"
	// ScreenSetup is journey step 10, the long unattended stretch.
	ScreenSetup Screen = "setup"
	// ScreenDone is journey step 12: the address, and the offer to remove the
	// installer.
	ScreenDone Screen = "done"
	// ScreenRemoving and ScreenRemoved are the other direction: taking remote
	// access down deliberately, which uninstalling RASA does not do.
	ScreenRemoving Screen = "removing"
	ScreenRemoved  Screen = "removed"
)

// StepState is how one line on a progress screen is rendered.
type StepState string

const (
	StepPending StepState = "pending"
	StepRunning StepState = "running"
	StepDone    StepState = "done"
	StepFailed  StepState = "failed"
	// StepSkipped covers a step that did not need to run — an address family
	// this connection does not have, a certificate that is still valid. It
	// reads differently from "done" on purpose: a user comparing their screen
	// to a friend's should be able to see that they took a different path.
	StepSkipped StepState = "skipped"
)

// Step is one line on a progress screen.
type Step struct {
	ID    string    `json:"id"`
	Label string    `json:"label"`
	State StepState `json:"state"`
	// Note is the short result shown beside a finished line. Plain language:
	// this is rendered verbatim.
	Note string `json:"note,omitempty"`
}

// DomainOption is one entry in the address dropdown.
type DomainOption struct {
	Name    string `json:"name"`
	Default bool   `json:"default,omitempty"`
}

// JellyfinView is what the sign-in screen knows.
type JellyfinView struct {
	Found bool `json:"found"`
	// Address is host:port, shown so the user can confirm RASA found the
	// right server when they run more than one.
	Address string `json:"address,omitempty"`
	Version string `json:"version,omitempty"`
	// ServerName is what the owner called it, which identifies the server far
	// better than an address does.
	ServerName string `json:"server_name,omitempty"`
	// SignedIn is true once an administrator account has been accepted.
	SignedIn bool   `json:"signed_in"`
	Username string `json:"username,omitempty"`
}

// NameView is the state of the hostname picker.
type NameView struct {
	Label  string `json:"label"`
	Parent string `json:"parent"`
	// Hostname is the full name once claimed.
	Hostname string `json:"hostname,omitempty"`
	// Availability is advisory (SPEC.md §8) and never states more than DNS
	// evidence supports.
	Availability string `json:"availability,omitempty"`
	Advice       string `json:"advice,omitempty"`
	// Suggestions are the same name on other parent domains, offered after a
	// collision.
	Suggestions []string `json:"suggestions,omitempty"`
}

// PortView is the state of journey step 9.
type PortView struct {
	// Needed is false when nothing has to be opened — mesh mode, or a
	// connection that is already direct.
	Needed bool `json:"needed"`
	// Open is true once a mapping exists, however it was made.
	Open bool `json:"open"`
	// Automatic records whether the router did it without the user.
	Automatic bool `json:"automatic"`
	// Permanent is false when the router capped the lease, which means the
	// mapping will lapse on a reboot and the manual instructions still apply.
	Permanent bool `json:"permanent"`
	External  int  `json:"external,omitempty"`
	Internal  int  `json:"internal,omitempty"`
	// RouterName is what RASA believes the router is, so the instructions can
	// name it rather than saying "your router".
	RouterName string `json:"router_name,omitempty"`
	// Instructions is the rendered guide, shown only when there is work to do.
	Instructions []GuideStep `json:"instructions,omitempty"`
	// Values are the exact fields to type. These are what users get wrong,
	// so they are presented filled in rather than as blanks (SPEC.md §6).
	Values []GuideValue `json:"values,omitempty"`
	// ReservationNote warns that this machine's address is leased and will
	// eventually move, breaking a static forward.
	ReservationNote string `json:"reservation_note,omitempty"`
}

// GuideStep is one instruction line.
type GuideStep struct {
	Text string `json:"text"`
}

// GuideValue is one field to fill in on a router page.
type GuideValue struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// ResultView is journey step 12.
type ResultView struct {
	URL string `json:"url,omitempty"`
	// Reachable is the outcome of the external check. Three-valued, because a
	// router without NAT hairpinning fails a self-check while working
	// perfectly for everyone outside the house.
	Reachable string `json:"reachable,omitempty"`
	// ReachMessage explains an inconclusive result without alarming anyone.
	ReachMessage string `json:"reach_message,omitempty"`
	// RecoveryFile is where the details were written, since RASA is about to
	// stop existing.
	RecoveryFile string `json:"recovery_file,omitempty"`
	LogFile      string `json:"log_file,omitempty"`
	// QRPNG is a data: URI for the address, for pointing a phone at.
	QRPNG string `json:"qr_png,omitempty"`
}

// Model is the whole UI state. It is a value: the UI renders a snapshot and
// never holds a reference into the wizard.
type Model struct {
	// Revision increments on every change, so a client can tell a stale
	// snapshot from a current one.
	Revision int    `json:"revision"`
	Screen   Screen `json:"screen"`
	Version  string `json:"version"`
	// Busy is true while a long operation is running; the UI disables its
	// controls rather than queueing a second one.
	Busy bool `json:"busy"`
	// Repair is true when a previous setup was found, which turns the welcome
	// screen into a repair screen (SPEC.md §9, step 4).
	Repair bool        `json:"repair"`
	Phase  state.Phase `json:"phase"`

	Checks []Step `json:"checks"`
	Setup  []Step `json:"setup"`
	// Removal is populated only while remote access is being taken down.
	Removal []Step `json:"removal,omitempty"`

	// Mode and Explanation report the branch the router took. The user is
	// told, not asked.
	Mode        state.Mode `json:"mode,omitempty"`
	Explanation string     `json:"explanation,omitempty"`
	ListenPort  int        `json:"listen_port,omitempty"`

	Jellyfin JellyfinView   `json:"jellyfin"`
	DynuKey  bool           `json:"dynu_key"`
	Domains  []DomainOption `json:"domains,omitempty"`
	Name     NameView       `json:"name"`
	Port     PortView       `json:"port"`
	Result   ResultView     `json:"result"`

	// Warnings are things that succeeded but will bite later. They persist
	// across screens because the user has to see them at the end, when RASA
	// is about to stop existing.
	Warnings []Warning `json:"warnings,omitempty"`

	// Err is the current failure, already reduced to its user-safe
	// projection. The technical detail went to the log and is structurally
	// unreachable from here.
	Err *rasaerr.UserFacing `json:"error,omitempty"`
}

// Warning is a user-facing caution carried on the model.
type Warning struct {
	Code string `json:"code"`
	Text string `json:"text"`
}

// clone deep-copies the slices so a subscriber cannot observe a later mutation
// through a snapshot it already holds.
func (m Model) clone() Model {
	out := m
	out.Checks = append([]Step(nil), m.Checks...)
	out.Setup = append([]Step(nil), m.Setup...)
	out.Removal = append([]Step(nil), m.Removal...)
	out.Domains = append([]DomainOption(nil), m.Domains...)
	out.Warnings = append([]Warning(nil), m.Warnings...)
	out.Name.Suggestions = append([]string(nil), m.Name.Suggestions...)
	out.Port.Instructions = append([]GuideStep(nil), m.Port.Instructions...)
	out.Port.Values = append([]GuideValue(nil), m.Port.Values...)
	if m.Err != nil {
		e := *m.Err
		e.Actions = append([]rasaerr.Action(nil), m.Err.Actions...)
		out.Err = &e
	}
	return out
}

// checkIDs and setupIDs name the progress lines. They are constants because
// the UI addresses them and because a typo would otherwise produce a line that
// silently never updates.
const (
	CheckJellyfin = "jellyfin"
	CheckInternet = "internet"
	CheckRouter   = "router"
	CheckPorts    = "ports"

	SetupAddress   = "address"
	SetupDNS       = "dns"
	SetupProxy     = "proxy"
	SetupCert      = "certificate"
	SetupJellyfin  = "jellyfin"
	SetupKeepAlive = "keepalive"
	SetupVerify    = "verify"
)

func initialChecks() []Step {
	return []Step{
		{ID: CheckJellyfin, Label: "Finding your Jellyfin server", State: StepPending},
		{ID: CheckInternet, Label: "Checking your internet connection", State: StepPending},
		{ID: CheckRouter, Label: "Asking your router what it can do", State: StepPending},
		{ID: CheckPorts, Label: "Checking this computer's ports", State: StepPending},
	}
}

func initialSetup() []Step {
	return []Step{
		{ID: SetupAddress, Label: "Publishing your web address", State: StepPending},
		{ID: SetupDNS, Label: "Waiting for the address to appear online", State: StepPending},
		{ID: SetupProxy, Label: "Installing the secure connection", State: StepPending},
		{ID: SetupCert, Label: "Getting your security certificate", State: StepPending},
		{ID: SetupJellyfin, Label: "Telling Jellyfin about its new address", State: StepPending},
		{ID: SetupKeepAlive, Label: "Keeping your address up to date", State: StepPending},
		{ID: SetupVerify, Label: "Checking it works from outside your network", State: StepPending},
	}
}

func domainOptions(c *domains.Catalog) []DomainOption {
	if c == nil {
		return nil
	}
	all := c.All()
	out := make([]DomainOption, len(all))
	for i, d := range all {
		out[i] = DomainOption{Name: d.Name, Default: d.Default}
	}
	return out
}
