package sep2embed

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// URL-index addressing on the bridge's served tree.
//
// Core owns URL and link construction; these tests assert the property
// end-to-end on the resources the BRIDGE seeds, over real mTLS, because the
// bridge is what stamps DERListLink, RegistrationLink, and the DER hrefs.
// They assert the served bytes rather than decoded structs, per
// [[data-invariants]] Rule 1: the bytes carry every href the server actually
// emitted, including ones a future change might add.

// lfdiShapedHref matches an href attribute whose value contains a run of 40
// or more hex characters: the shape of an IEEE 2030.5 LFDI (section 6.3.4
// fixes the canonical length at 40; the open upper bound also catches a
// longer identifier being spliced in).
var lfdiShapedHref = regexp.MustCompile(`href="[^"]*[0-9A-Fa-f]{40,}[^"]*"`)

// TestURLIndexNoLFDIInAnyServedHref is the regression guard against a
// half-converted link set. A client that follows links lands on a 404 if some
// hrefs kept the LFDI form while the routes moved to the index, and that
// failure is invisible to any test that only checks the links it thought to
// name.
//
// The walk is driven by discovery: it starts at the device's own EndDevice
// resource and follows the links found there, which is what a conformant
// client does, so a link this test never hardcoded is still inspected.
//
// The assertion is negative, so it is paired with a positive control: the
// walk must have visited several documents AND seen at least one index-form
// /edev href, otherwise an empty tree would pass vacuously.
func TestURLIndexNoLFDIInAnyServedHref(t *testing.T) {
	t.Parallel()

	baseURL, devices := newMUPTestServer(t, "urlindex-a", "urlindex-b")
	dev := devices[0]

	if dev.edevID == dev.lfdi {
		t.Fatalf("device is still addressed by its LFDI (%q); addressing did not move to an index", dev.edevID)
	}

	paths := []string{
		"/dcap",
		"/edev",
		"/edev/" + dev.edevID,
		"/edev/" + dev.edevID + "/rg",
		"/edev/" + dev.edevID + "/der",
		"/edev/" + dev.edevID + "/der/1",
		"/edev/" + dev.edevID + "/der/1/dercap",
		"/edev/" + dev.edevID + "/fsa",
		"/sdev",
		"/mup",
	}

	visited := 0
	sawIndexHref := false
	for _, p := range paths {
		status, body := getSEP2(t, dev, baseURL+p)
		if status != http.StatusOK {
			// Not every resource is seeded for every device; a non-200 carries
			// no hrefs to inspect and is not what this test is about.
			continue
		}
		visited++
		text := string(body)

		if m := lfdiShapedHref.FindString(text); m != "" {
			t.Errorf("GET %s served an LFDI-shaped value inside an href: %s\nbody: %s", p, m, text)
		}
		// Belt and braces against a future LFDI form that escapes the length
		// regexp: neither device's literal LFDI may appear in an href.
		for _, d := range devices {
			if strings.Contains(text, `href="/edev/`+d.lfdi) {
				t.Errorf("GET %s served an LFDI-addressed href for %s\nbody: %s", p, d.lfdi, text)
			}
		}
		if strings.Contains(text, `href="/edev/`+dev.edevID) {
			sawIndexHref = true
		}
	}

	if visited < 4 {
		t.Fatalf("walked only %d served documents; the negative guard would pass vacuously", visited)
	}
	if !sawIndexHref {
		t.Fatal("no index-form /edev href was seen anywhere in the walked tree; the negative guard would pass vacuously")
	}
}

// TestURLIndexSeededLinksAreIndexFormAndResolve asserts the exact href
// strings the bridge stamps during seeding, and that a GET on each reaches a
// real route. An href that is converted but does not resolve is the same
// broken-link failure as one that was never converted.
func TestURLIndexSeededLinksAreIndexFormAndResolve(t *testing.T) {
	t.Parallel()

	baseURL, devices := newMUPTestServer(t, "urlindex-links-a")
	dev := devices[0]
	id := dev.edevID

	status, body := getSEP2(t, dev, baseURL+"/edev/"+id)
	if status != http.StatusOK {
		t.Fatalf("GET /edev/%s status = %d, want 200; body=%s", id, status, body)
	}
	text := string(body)

	// Identity is still the LFDI and is still advertised as a field value.
	// Only addressing moved, so this must remain present.
	if !strings.Contains(text, dev.lfdi) {
		t.Errorf("served EndDevice does not carry its LFDI %q as identity:\n%s", dev.lfdi, text)
	}

	wantLinks := map[string]string{
		"RegistrationLink": `<RegistrationLink href="/edev/` + id + `/rg"`,
		"DERListLink":      `<DERListLink `,
	}
	for name, want := range wantLinks {
		if !strings.Contains(text, want) {
			t.Errorf("served EndDevice is missing %s in index form (want %q):\n%s", name, want, text)
		}
	}
	if want := `href="/edev/` + id + `/der"`; !strings.Contains(text, want) {
		t.Errorf("DERListLink href is not %q:\n%s", want, text)
	}

	// Each advertised link must resolve.
	for _, p := range []string{
		"/edev/" + id + "/rg",
		"/edev/" + id + "/der",
		"/edev/" + id + "/der/1/dercap",
	} {
		status, body := getSEP2(t, dev, baseURL+p)
		if status != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200 (an advertised link must resolve); body=%s", p, status, body)
		}
	}
}

// TestURLIndexStaleIndexPointingAtAnotherDeviceIs403 is the safety net for
// the whole scheme. Indices are assigned per process, so a client holding a
// URL learned from an earlier run can find that the index now addresses a
// DIFFERENT device. That must be denied on the caller's certificate, never
// served: a silent cross-device read would be strictly worse than a broken
// link, and worse than either URL scheme.
//
// This is the bridge-side counterpart of the core test of the same name. It
// matters here because the bridge is where the ownership gate
// (acl.go's storeOwnerResolver) actually runs.
func TestURLIndexStaleIndexPointingAtAnotherDeviceIs403(t *testing.T) {
	t.Parallel()

	baseURL, devices := newMUPTestServer(t, "urlindex-stale-a", "urlindex-stale-b")
	deviceA, deviceB := devices[0], devices[1]

	if deviceA.edevID == deviceB.edevID {
		t.Fatalf("both devices share URL index %q; the fixture cannot test cross-device access", deviceA.edevID)
	}

	// Device A presents its own certificate against device B's index, which
	// is exactly the shape of a stale URL that has been reassigned.
	stale := baseURL + "/edev/" + deviceB.edevID + "/rg"
	status, body := getSEP2(t, deviceA, stale)
	if status != http.StatusForbidden {
		t.Errorf("GET %s as device A: status = %d, want 403; body=%s", stale, status, body)
	}
	text := string(body)
	for _, leak := range []struct {
		what  string
		found bool
	}{
		{"a Registration document", strings.Contains(text, "<Registration")},
		{"a PIN element", strings.Contains(text, "pIN")},
		{"any XML document", strings.Contains(text, "<?xml")},
		{"the owning device's LFDI", strings.Contains(text, deviceB.lfdi)},
	} {
		if leak.found {
			t.Errorf("denied response leaked %s; body=%q", leak.what, text)
		}
	}

	// The gate denies by ownership, not by breaking the route: the device
	// that does own that index is still served.
	status, ownBody := getSEP2(t, deviceB, stale)
	if status != http.StatusOK {
		t.Errorf("owner GET %s: status = %d, want 200; body=%s", stale, status, ownBody)
	}
}

// TestURLIndexSeedingIsDeterministicWithinARun asserts that seeding the same
// fleet twice assigns the same indices. reg.Snapshot() iterates a Go map,
// whose order is randomized per process, so without the sort in seedStores
// the same fleet would produce different URLs on every run.
func TestURLIndexSeedingIsDeterministicWithinARun(t *testing.T) {
	t.Parallel()

	mrids := []string{"mrid-z-last", "mrid-a-first", "mrid-m-middle", "mrid-b-second"}

	seedAndCapture := func() map[string]string {
		t.Helper()
		reg := fixtureRegistryFor(t, mrids)
		stores := newStores()
		if err := seedStores(t.Context(), stores, reg, seedPolicy{resolvePIN: testResolvePIN}); err != nil {
			t.Fatalf("seedStores: %v", err)
		}
		got := make(map[string]string, len(mrids))
		for _, m := range mrids {
			got[m] = urlIndexFor(t, stores, m)
		}
		return got
	}

	first := seedAndCapture()
	for i := 0; i < 5; i++ {
		again := seedAndCapture()
		for _, m := range mrids {
			if first[m] != again[m] {
				t.Fatalf("mRID %q got index %q then %q across two seedings of the same fleet; assignment is not deterministic",
					m, first[m], again[m])
			}
		}
	}

	// Sorted by mRID, so the alphabetically first device holds index 1. This
	// pins the ordering rule itself, not merely that it is repeatable: a
	// change to insertion order would otherwise stay invisible.
	if got := first["mrid-a-first"]; got != "1" {
		t.Errorf("mrid-a-first index = %q, want %q (assignment walks mRIDs in sorted order)", got, "1")
	}
	if got := first["mrid-z-last"]; got != "4" {
		t.Errorf("mrid-z-last index = %q, want %q (assignment walks mRIDs in sorted order)", got, "4")
	}
}

// TestNewStoresWiresNonNilEndDeviceIndex guards the one field whose absence
// is now a BOOT-TIME PANIC rather than a silent degradation.
//
// core's enddevice.HandleCreateEndDevice panics at construction on a nil
// EndDeviceIndexer (core 584b02a), deliberately, so a mis-wired server fails
// loudly at assembly instead of returning a recovered 500 on every POST /edev.
// assembly.BuildProtocolRouter substitutes a process-local allocator when
// Stores.EndDeviceIndexes is nil, so the bridge would not actually trip that
// panic today; what it WOULD do is silently lose the index assignments seeding
// depends on, because seedOne allocates from stores.EndDeviceIndexes directly
// and would nil-panic there first.
//
// Either way the failure is at startup and far from the edit that caused it.
// Assert the wiring here, where the cause is named.
func TestNewStoresWiresNonNilEndDeviceIndex(t *testing.T) {
	t.Parallel()

	stores := newStores()
	if stores.EndDeviceIndexes == nil {
		t.Fatal("newStores() left Stores.EndDeviceIndexes nil: seeding would nil-panic at boot, and a nil indexer reaching core's HandleCreateEndDevice panics at router construction")
	}

	// A usable allocator, not merely a non-nil pointer.
	idx, err := stores.EndDeviceIndexes.Allocate("mrid-wiring-probe")
	if err != nil {
		t.Fatalf("Allocate on the wired index: %v", err)
	}
	if idx != "1" {
		t.Errorf("first allocation from a fresh store = %q, want %q", idx, "1")
	}
}
