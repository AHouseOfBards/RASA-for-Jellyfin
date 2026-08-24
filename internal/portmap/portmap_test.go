package portmap

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/AHouseOfBards/RASA-for-Jellyfin/internal/logging"
)

const svcType = "urn:schemas-upnp-org:service:WANIPConnection:1"

func okReply(action string) string {
	return `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:` + action + `Response xmlns:u="` + svcType + `"/></s:Body></s:Envelope>`
}

func getReply(internalPort, client, lease string) string {
	return `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:GetSpecificPortMappingEntryResponse xmlns:u="` + svcType + `">` +
		`<NewInternalPort>` + internalPort + `</NewInternalPort>` +
		`<NewInternalClient>` + client + `</NewInternalClient>` +
		`<NewEnabled>1</NewEnabled>` +
		`<NewPortMappingDescription>RASA for Jellyfin</NewPortMappingDescription>` +
		`<NewLeaseDuration>` + lease + `</NewLeaseDuration>` +
		`</u:GetSpecificPortMappingEntryResponse></s:Body></s:Envelope>`
}

func faultReply(code, desc string) string {
	return `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail>` +
		`<UPnPError xmlns="urn:schemas-upnp-org:control-1-0">` +
		`<errorCode>` + code + `</errorCode><errorDescription>` + desc + `</errorDescription>` +
		`</UPnPError></detail></s:Fault></s:Body></s:Envelope>`
}

// router is a scriptable fake IGD. handler receives the SOAPAction name and
// request body and returns (status, body).
func router(t *testing.T, handler func(action, body string) (int, string)) *Mapper {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		action := r.Header.Get("SOAPAction")
		if i := strings.LastIndex(action, "#"); i >= 0 {
			action = strings.Trim(action[i+1:], `"`)
		}
		status, body := handler(action, string(b))
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, svcType, logging.Discard())
}

func req() Request {
	return Request{
		ExternalPort:   443,
		InternalPort:   443,
		InternalClient: netip.MustParseAddr("192.168.1.10"),
		Protocol:       TCP,
	}
}

func TestAddRequestsPermanentLeaseFirst(t *testing.T) {
	var leases []string
	m := router(t, func(action, body string) (int, string) {
		if action == "AddPortMapping" {
			leases = append(leases, between(body, "<NewLeaseDuration>", "</NewLeaseDuration>"))
			return 200, okReply(action)
		}
		return 200, getReply("443", "192.168.1.10", "0")
	})

	res, err := m.Add(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0] != "0" {
		t.Fatalf("expected one request with a permanent lease, got %v", leases)
	}
	if !res.Mapping.Permanent() {
		t.Error("mapping should be permanent")
	}
	if !res.PermanentRequested {
		t.Error("PermanentRequested should be true")
	}
}

func TestAddFallsBackToFiniteLease(t *testing.T) {
	// Some routers reject a permanent lease outright. That is a downgrade, not
	// a failure — but the caller must be able to tell, because it decides
	// whether the user is offered a static forward instead.
	var leases []string
	m := router(t, func(action, body string) (int, string) {
		switch action {
		case "AddPortMapping":
			l := between(body, "<NewLeaseDuration>", "</NewLeaseDuration>")
			leases = append(leases, l)
			if l == "0" {
				return 500, faultReply("402", "Invalid Args")
			}
			return 200, okReply(action)
		default:
			return 200, getReply("443", "192.168.1.10", "604800")
		}
	})

	res, err := m.Add(context.Background(), req())
	if err != nil {
		t.Fatalf("should have recovered with a finite lease: %v", err)
	}
	if len(leases) != 2 || leases[0] != "0" {
		t.Fatalf("expected permanent then finite, got %v", leases)
	}
	if res.PermanentRequested {
		t.Error("PermanentRequested should be false after the downgrade")
	}
	if res.Mapping.Permanent() {
		t.Error("mapping must not claim to be permanent")
	}
}

func TestAddRetriesOnOnlyPermanentLeasesSupported(t *testing.T) {
	// 725 means the opposite of how it reads: the router supports *only*
	// permanent leases. It still warrants a second attempt.
	e := &UPnPError{Code: errOnlyPermanentLeases}
	if !e.Retryable() {
		t.Fatal("725 should be retryable")
	}
}

func TestAddSurfacesConflict(t *testing.T) {
	// The one failure a user can act on: the port belongs to another device.
	m := router(t, func(action, body string) (int, string) {
		return 500, faultReply("718", "ConflictInMappingEntry")
	})

	_, err := m.Add(context.Background(), req())
	if err == nil {
		t.Fatal("expected a conflict error")
	}
	var ue *UPnPError
	if !errors.As(err, &ue) {
		t.Fatalf("not a UPnPError: %v", err)
	}
	if !ue.IsConflict() {
		t.Errorf("code %d should be reported as a conflict", ue.Code)
	}
	if ue.Retryable() {
		t.Error("a conflict must not be retried with a different lease")
	}
}

func TestAddVerifiesByReadback(t *testing.T) {
	// Routers do report success and then do nothing. The read-back is what
	// catches that, so a mapping that cannot be confirmed must say so.
	m := router(t, func(action, body string) (int, string) {
		if action == "AddPortMapping" {
			return 200, okReply(action)
		}
		return 500, faultReply("714", "NoSuchEntryInArray")
	})

	res, err := m.Add(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	if res.VerifiedByReadback {
		t.Error("router reported no mapping, so it must not be marked verified")
	}
}

func TestAddDetectsDowngradeByReadback(t *testing.T) {
	// The router accepted lease 0 but silently stored a finite one. Only the
	// read-back reveals it.
	m := router(t, func(action, body string) (int, string) {
		if action == "AddPortMapping" {
			return 200, okReply(action)
		}
		return 200, getReply("443", "192.168.1.10", "3600")
	})

	res, err := m.Add(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	if res.Mapping.Permanent() {
		t.Fatal("read-back showed a finite lease; mapping must not claim permanence")
	}
	if !res.PermanentRequested {
		t.Error("a permanent lease was requested, so that should be recorded")
	}
	if res.Mapping.LeaseSeconds != 3600 {
		t.Errorf("lease = %d, want the value the router reported", res.Mapping.LeaseSeconds)
	}
}

func TestGetReturnsFalseForMissingMapping(t *testing.T) {
	m := router(t, func(action, body string) (int, string) {
		return 500, faultReply("714", "NoSuchEntryInArray")
	})

	_, ok, err := m.Get(context.Background(), 443, TCP)
	if err != nil {
		t.Fatalf("a missing mapping is a normal answer, not an error: %v", err)
	}
	if ok {
		t.Error("ok should be false")
	}
}

func TestGetParsesMapping(t *testing.T) {
	m := router(t, func(action, body string) (int, string) {
		return 200, getReply("8096", "192.168.1.10", "0")
	})

	got, ok, err := m.Get(context.Background(), 443, TCP)
	if err != nil || !ok {
		t.Fatalf("err=%v ok=%v", err, ok)
	}
	if got.InternalPort != 8096 || got.InternalClient != "192.168.1.10" {
		t.Fatalf("parsed wrong: %+v", got)
	}
	if !got.Enabled || !got.Permanent() {
		t.Fatalf("flags wrong: %+v", got)
	}
}

func TestDeleteAbsentMappingSucceeds(t *testing.T) {
	m := router(t, func(action, body string) (int, string) {
		return 500, faultReply("714", "NoSuchEntryInArray")
	})
	if err := m.Delete(context.Background(), 443, TCP); err != nil {
		t.Fatalf("deleting an absent mapping should succeed: %v", err)
	}
}

func TestDeleteSendsCorrectPort(t *testing.T) {
	var seen string
	m := router(t, func(action, body string) (int, string) {
		if action == "DeletePortMapping" {
			seen = between(body, "<NewExternalPort>", "</NewExternalPort>")
		}
		return 200, okReply(action)
	})
	if err := m.Delete(context.Background(), 8443, TCP); err != nil {
		t.Fatal(err)
	}
	if seen != "8443" {
		t.Fatalf("external port sent as %q", seen)
	}
}

func TestAddRejectsMissingInternalClient(t *testing.T) {
	m := router(t, func(action, body string) (int, string) { return 200, okReply(action) })
	r := req()
	r.InternalClient = netip.Addr{}
	if _, err := m.Add(context.Background(), r); err == nil {
		t.Fatal("an invalid internal client must be rejected before any request")
	}
}

func TestUnavailableMapperRefusesWork(t *testing.T) {
	m := New("", "", logging.Discard())
	if m.Available() {
		t.Fatal("a mapper with no control URL is not available")
	}
	if _, err := m.Add(context.Background(), req()); err == nil {
		t.Error("Add should refuse")
	}
	if err := m.Delete(context.Background(), 443, TCP); err == nil {
		t.Error("Delete should refuse")
	}
}

func TestDescriptionIsRecognisable(t *testing.T) {
	// A user looking at their router a year from now should be able to tell
	// what created the entry.
	var seen string
	m := router(t, func(action, body string) (int, string) {
		if action == "AddPortMapping" {
			seen = between(body, "<NewPortMappingDescription>", "</NewPortMappingDescription>")
		}
		return 200, getReply("443", "192.168.1.10", "0")
	})
	if _, err := m.Add(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "Jellyfin") {
		t.Fatalf("description %q does not identify the app", seen)
	}
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		return ""
	}
	return s[:j]
}
