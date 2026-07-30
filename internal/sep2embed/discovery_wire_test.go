package sep2embed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2srv/assembly"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// discoveryWireLFDI is the single device the served-bytes tests below use.
const discoveryWireLFDI = "AAAA00000000000000000000000000000000AAAA"

// serveDiscoveryChain seeds one device, applies one control delta, and
// returns a handler over the REAL protocol router, plus a get function
// that performs an authenticated GET as that device.
//
// The tests in this file exercise the served HTTP response, not the store
// contents, because the store-level tests in discovery_test.go cannot see
// two failure classes that actually stopped the reference client:
//
//  1. A record present in the store but not reachable through any mounted
//     route (core mounts a function set's routes only when its store is
//     non-nil, and the route shapes are core's, not this package's).
//  2. A list resource whose all/results attributes disagree with the
//     records it contains. all="0" with a record present is precisely the
//     symptom measured against the reference client: it reads the list
//     attribute, not the child count.
func serveDiscoveryChain(t *testing.T) (func(t *testing.T, path string) (int, string), *assembly.Stores) {
	t.Helper()

	reg := registry.New()
	if err := reg.AddBatch([]registry.Entry{
		{MRID: "mrid-inv-1", Name: "Inverter 1", LFDI: discoveryWireLFDI, Placeholder: true},
	}); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	ctx := context.Background()
	stores := newStores()
	pin := uint32(111115)
	pollRate := uint32(300)
	if err := seedStores(ctx, stores, reg, nil, &pin, &pollRate); err != nil {
		t.Fatalf("seedStores: %v", err)
	}

	// One delta, so the DERControl the client is meant to discover exists.
	if err := ApplyControlDelta(ctx, stores, nil, reg,
		sep2.DefaultDERControl{DERControlBase: &sep2.DERControlBase{}},
		ControlDelta{
			Object:    "mrid-inv-1",
			Attribute: "DERControl.DERControlBase.opModTargetW",
			Value:     sep2.ActivePower{Multiplier: 0, Value: 5000},
		}); err != nil {
		t.Fatalf("ApplyControlDelta: %v", err)
	}

	h := buildHandler(assembly.RouterConfig{}, stores, reg,
		sep2srv.Identity{LFDI: "SERVER-LFDI", SFDI: "1"}, nil, telemetryConfig{}, nil)

	get := func(t *testing.T, path string) (int, string) {
		t.Helper()
		req := withIdentity(httptest.NewRequest(http.MethodGet, path, nil), discoveryWireLFDI, "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	return get, stores
}

// TestDiscoveryChainIsServedEndToEnd walks the exact hop sequence a
// link-traversing client walks, over the real router: EndDevice ->
// Registration, and EndDevice -> FSA list -> FSA -> DERProgram list ->
// DERControl list. Every hop must return 200 with the 2030.5 media type,
// because a single 404 anywhere in the chain ends the walk.
func TestDiscoveryChainIsServedEndToEnd(t *testing.T) {
	t.Parallel()

	get, _ := serveDiscoveryChain(t)
	base := "/edev/" + discoveryWireLFDI

	for _, path := range []string{
		base,
		base + "/rg",
		base + "/fsa",
		base + "/fsa/" + controlFSAID,
		derProgramListHref(discoveryWireLFDI, controlFSAID),
		derProgramListHref(discoveryWireLFDI, controlFSAID) + "/" + controlDERProgramID + "/derc",
	} {
		code, body := get(t, path)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200; the client's walk stops here\nbody: %s", path, code, body)
			continue
		}
		if !strings.Contains(body, `xmlns="`+sep2.Namespace+`"`) {
			t.Errorf("GET %s served a body without the 2030.5 namespace:\n%s", path, body)
		}
	}
}

// TestServedEndDeviceCarriesDiscoveryLinks asserts the two links on the
// SERVED EndDevice, not on the stored struct. Core serves the stored
// record, so a link that exists in memory but is dropped in
// serialization would still fail the client.
func TestServedEndDeviceCarriesDiscoveryLinks(t *testing.T) {
	t.Parallel()

	get, _ := serveDiscoveryChain(t)
	code, body := get(t, "/edev/"+discoveryWireLFDI)
	if code != http.StatusOK {
		t.Fatalf("GET /edev = %d: %s", code, body)
	}

	if !strings.Contains(body, `<RegistrationLink href="/edev/`+discoveryWireLFDI+`/rg"`) {
		t.Errorf("served EndDevice has no RegistrationLink; the reference client fails registration outright:\n%s", body)
	}
	if !strings.Contains(body, `<FunctionSetAssignmentsListLink href="/edev/`+discoveryWireLFDI+`/fsa" all="1"`) {
		t.Errorf("served EndDevice has no FunctionSetAssignmentsListLink with all=1:\n%s", body)
	}
	// changedTime is a required element; a zero value would advertise 1970.
	if strings.Contains(body, "<changedTime>0</changedTime>") {
		t.Errorf("served EndDevice changedTime is 0:\n%s", body)
	}
}

// TestServedFSAListReportsOneResult is the direct regression pin for the
// measured symptom: GET /edev/{lfdi}/fsa returned all="0". A client reads
// the list's own all attribute, so all="0" ends the walk even if a record
// exists underneath.
func TestServedFSAListReportsOneResult(t *testing.T) {
	t.Parallel()

	get, _ := serveDiscoveryChain(t)
	code, body := get(t, "/edev/"+discoveryWireLFDI+"/fsa")
	if code != http.StatusOK {
		t.Fatalf("GET /edev/{lfdi}/fsa = %d: %s", code, body)
	}

	if strings.Contains(body, `all="0"`) {
		t.Errorf("FSA list still serves all=\"0\"; a client's list walk dead-ends here:\n%s", body)
	}
	if !strings.Contains(body, `all="1"`) || !strings.Contains(body, `results="1"`) {
		t.Errorf("FSA list must serve all=\"1\" results=\"1\":\n%s", body)
	}
	// The child FSA must actually be inlined, with the link that carries
	// the walk onward: a list whose attributes claim one result but whose
	// body is empty is the same dead end one level down.
	if !strings.Contains(body, "<FunctionSetAssignments ") {
		t.Errorf("FSA list claims a result but inlines no FunctionSetAssignments:\n%s", body)
	}
	if !strings.Contains(body, `<DERProgramListLink href="`+derProgramListHref(discoveryWireLFDI, controlFSAID)+`"`) {
		t.Errorf("inlined FSA has no DERProgramListLink to the control path's own program list:\n%s", body)
	}
}

// TestServedDERControlCarriesActivatableInterval asserts the interval on
// the bytes a client actually receives from the list it polls. A client's
// scheduler derives the event window from these two values and discards a
// window that has already ended, so their presence in the served list is
// the property that decides whether the control ever activates.
func TestServedDERControlCarriesActivatableInterval(t *testing.T) {
	t.Parallel()

	get, stores := serveDiscoveryChain(t)
	listPath := derProgramListHref(discoveryWireLFDI, controlFSAID) + "/" + controlDERProgramID + "/derc"
	code, body := get(t, listPath)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", listPath, code, body)
	}

	if !strings.Contains(body, "<interval>") {
		t.Errorf("served DERControl list carries no <interval>; a scheduler computes end=0 and drops the block:\n%s", body)
	}
	if strings.Contains(body, "<duration>0</duration>") {
		t.Errorf("served DERControl has duration 0, so it expires the instant it starts:\n%s", body)
	}

	// Cross-check the served bytes against the stored record, so this test
	// fails if serialization ever drops the interval that the store holds.
	scope := derControlScope(discoveryWireLFDI, controlFSAID, controlDERProgramID)
	ctrl, err := stores.DERControls.Get(context.Background(), scope, activeControlID)
	if err != nil {
		t.Fatalf("DERControls.Get: %v", err)
	}
	if ctrl.Interval == nil {
		t.Fatal("stored DERControl has no interval")
	}
	if !strings.Contains(body, "<start>"+strconv.FormatInt(ctrl.Interval.Start, 10)+"</start>") {
		t.Errorf("served interval start does not match the stored %d:\n%s", ctrl.Interval.Start, body)
	}
}

// TestServedResourcesCarryWireLegalMRIDs asserts every mRID in the served
// discovery chain is a legal mRIDType value. This is the assertion that
// catches the whole class at once: a schema-driven client rejects the
// ENTIRE document on one malformed simple value, so a single illegal mRID
// anywhere in the chain silently removes that resource from the client's
// view even though the server returned 200.
func TestServedResourcesCarryWireLegalMRIDs(t *testing.T) {
	t.Parallel()

	get, _ := serveDiscoveryChain(t)
	base := "/edev/" + discoveryWireLFDI
	derpList := derProgramListHref(discoveryWireLFDI, controlFSAID)

	for _, path := range []string{
		base + "/fsa",
		base + "/fsa/" + controlFSAID,
		derpList,
		derpList + "/" + controlDERProgramID + "/derc",
		derpList + "/" + controlDERProgramID + "/dderc",
	} {
		code, body := get(t, path)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d: %s", path, code, body)
			continue
		}
		mrids := extractElements(body, "mRID")
		if len(mrids) == 0 {
			t.Errorf("GET %s served no mRID at all; mRID is a required element on these resources:\n%s", path, body)
			continue
		}
		for _, m := range mrids {
			if !isMRIDLegal(m) {
				t.Errorf("GET %s served mRID %q, which is not a legal mRIDType (hexBinary, <= %d hex chars); a schema-driven client discards the whole document",
					path, m, mridHexChars)
			}
		}
	}
}

// extractElements returns the text content of every <name>...</name>
// element in body. Deliberately a crude scan rather than a real XML
// decode: these tests assert on the bytes as a foreign parser sees them,
// so round-tripping through Go's decoder would hide exactly the
// serialization-level defects being checked.
func extractElements(body, name string) []string {
	openTag, closeTag := "<"+name+">", "</"+name+">"
	var out []string
	for {
		i := strings.Index(body, openTag)
		if i < 0 {
			return out
		}
		body = body[i+len(openTag):]
		j := strings.Index(body, closeTag)
		if j < 0 {
			return out
		}
		out = append(out, body[:j])
		body = body[j+len(closeTag):]
	}
}
