package sep2embed

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim/diff"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/registry"
)

// wantReplyTo and wantResponseRequired are the wire values core stamps onto
// a DERControl that carries neither field (IEEECORE-067, core v0.12.1:
// handlers/der.StampResponseRequest, whose default is
// handlers/response.ListHref(DefaultSetID) and DefaultResponseRequired
// 0x07).
//
// They are written here as literal wire strings rather than computed from
// core's own constants on purpose. Computing them would make this test
// agree with core by construction and pass no matter what core stamped,
// which is the failure mode where a test tracks the implementation instead
// of the contract. 0x07 sets all three IEEE 2030.5 Table 32 bits (message
// received, specific response, response on transition), which is the value
// CSIP CTP CORE-022 names.
//
// Both are ATTRIBUTES, not child elements. sep.xsd declares them on
// complexType RespondableResource as
// `xs:attribute name="replyTo" type="xs:anyURI"` (sep.xsd:5435) and
// `xs:attribute name="responseRequired" default="00" type="HexBinary8"`
// (sep.xsd:5440). These literals were previously written in child-element
// form, which pinned the core v0.12.0 serialization defect fixed by
// IEEECORE-103 as though it were the contract: the suite stayed green
// while every served DERControl was unparseable to a schema-following
// client. The anchor for these strings is the XSD above, not core's
// output.
const (
	wantReplyTo          = `replyTo="/rsps/1/rsp"`
	wantResponseRequired = `responseRequired="07"`
)

// TestServedDERControlCarriesStampedResponseRequest answers the question
// IEEECORE-067's card left open for this bridge: whether the bridge's own
// DERControl seeding has to carry replyTo and responseRequired itself, or
// whether core stamping them at serve time is sufficient.
//
// The answer this test pins is "core alone is sufficient, because the
// bridge writes neither field". That has two halves, and both are asserted,
// because either one alone would be satisfiable by the wrong code:
//
//  1. The STORED control carries neither field. This is the half that
//     matters for the future. Core's stamp is a fill-absent default, not an
//     override: a stored responseRequired of 00 means "explicitly no
//     response wanted" and SURVIVES the stamp. So a bridge that started
//     writing a zero value would silently suppress the response the CTP
//     requires, and the served bytes would still look structurally fine.
//     Asserting absence at the store catches that at its source.
//
//  2. The SERVED bytes carry both, on the single-resource route and on the
//     list route. Asserting the served bytes rather than a decoded struct
//     is deliberate: responseRequired is a HexBinary8 that has to reach the
//     wire as "07" and not as a decimal 7, and a decoded assertion cannot
//     tell those apart.
//
// The stamped replyTo is also dereferenced rather than taken on faith: an
// event advertising a URI that 404s is worse than one advertising none, and
// the bridge's own ACL table (internal/sep2acl) sits between a client and
// that URI. See TestDERLinksResolveOverRealServer for the same discipline
// applied to the DER links.
func TestServedDERControlCarriesStampedResponseRequest(t *testing.T) {
	t.Parallel()

	certDir := t.TempDir()

	_, _, caFile, err := ensureServerIdentity(certDir)
	if err != nil {
		t.Fatalf("ensureServerIdentity: %v", err)
	}
	caCertPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("read ca.pem: %v", err)
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(certDir, caKeyFileName))
	if err != nil {
		t.Fatalf("read ca-key.pem: %v", err)
	}
	caCert, caKey, err := parseCAPair(caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatalf("parseCAPair: %v", err)
	}

	certA, keyA, lfdiA := mintDeviceIdentity(t, caCert, caKey, "test-serial-rspreq-a")

	reg := registry.New()
	if err := reg.AddBatch([]registry.Entry{
		{MRID: "mrid-rspreq-a", Name: "Device RspReq A", LFDI: lfdiA},
	}); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	e, err := New(ctx, Config{
		Addr:                   "127.0.0.1:0",
		CertDir:                certDir,
		ResolveRegistrationPIN: testResolvePIN,
		ShutdownTimeout:        time.Second,
	}, reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErr:
			if err != nil {
				t.Errorf("Run returned error after ctx cancel: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Run did not return within 3s of ctx cancel (goroutine leak or unbounded shutdown)")
		}
	})

	// Create the control through the real DOWN path rather than by writing
	// the store directly, so what is asserted below is the shape
	// ApplyControlDelta actually produces in production.
	if err := ApplyControlDelta(ctx, e.stores, nil, reg, testControlPolicy, diff.Difference{
		Object:    "mrid-rspreq-a",
		Attribute: "DERControl.DERControlBase.opModTargetW",
		Value:     map[string]any{"multiplier": 0.0, "value": 5000.0},
	}); err != nil {
		t.Fatalf("ApplyControlDelta: %v", err)
	}

	edevA := embedURLIndex(t, e, "mrid-rspreq-a")
	scope := derControlScope(edevA, controlFSAID, controlDERProgramID)

	stored, err := e.stores.DERControls.Get(ctx, scope, activeControlID)
	if err != nil {
		t.Fatalf("DERControls.Get: %v", err)
	}
	if stored.ReplyTo != "" {
		t.Errorf("stored DERControl.ReplyTo = %q, want empty: the bridge must leave replyTo absent so core's serve-time stamp fills it with a URI core itself routes", stored.ReplyTo)
	}
	if stored.ResponseRequired != nil {
		t.Errorf("stored DERControl.ResponseRequired = %#02x, want nil (absent): a stored value survives core's fill-absent stamp, and a stored 00 would suppress the response CSIP CTP CORE-022 requires", *stored.ResponseRequired)
	}

	baseURL := "https://" + e.Addr()
	clientA := deviceClient(t, certA, keyA, caCertPEM)

	dercBase := baseURL + "/edev/" + edevA + "/fsa/" + controlFSAID + "/derp/" + controlDERProgramID + "/derc"

	for _, tc := range []struct {
		name string
		url  string
	}{
		{"single-resource route", dercBase + "/" + activeControlID},
		{"list route", dercBase},
	} {
		desc := "device A GETting its own DERControl " + tc.name
		status, body := getBody(t, clientA, tc.url, desc)
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200\nbody: %s", desc, status, body)
		}
		if !strings.Contains(body, wantReplyTo) {
			t.Errorf("%s: served DERControl body does not contain %s\nbody: %s", tc.name, wantReplyTo, body)
		}
		if !strings.Contains(body, wantResponseRequired) {
			t.Errorf("%s: served DERControl body does not contain %s\nbody: %s", tc.name, wantResponseRequired, body)
		}
	}

	// The stamped replyTo must resolve through this bridge's own ACL table,
	// not just be syntactically present.
	assertStatus(t, clientA, http.MethodGet, baseURL+"/rsps/1/rsp", nil, http.StatusOK,
		"device A GETting the ResponseList named by the stamped replyTo")
	assertStatus(t, clientA, http.MethodGet, baseURL+"/rsps/1", nil, http.StatusOK,
		"device A GETting the ResponseSet that owns the stamped replyTo")
}
