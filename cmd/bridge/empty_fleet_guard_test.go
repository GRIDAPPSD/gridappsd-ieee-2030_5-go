package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/cim"
	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

// captureLog redirects the standard library's global log writer into a
// buffer for the duration of the test and restores it on cleanup. The
// tests that use this helper deliberately do NOT call t.Parallel(): Go's
// test driver runs every non-parallel top-level test to completion
// before any parallel-declared test in this package resumes its body
// (a t.Parallel() call only parks the test; it does not start running
// concurrently until the whole package's sequential invocation pass
// finishes), so a non-parallel caller of this helper never observes log
// output from this package's other (parallel) bootstrapRegistry tests.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return &buf
}

// emptyEnvelope is a valid-but-zero-row SPARQL response, the shape a
// load-modeled feeder's PowerElectronicsConnection enumeration query
// returns: no error, no incomplete-response flag, just zero bindings.
func emptyEnvelope(t *testing.T) []byte {
	t.Helper()
	env := map[string]any{
		"data": map[string]any{
			"head":    map[string]any{"vars": []string{"id", "name"}},
			"results": map[string]any{"bindings": []map[string]any{}},
		},
		"responseComplete": true,
		"id":               "x",
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return b
}

// countEnvelope builds the single-row {"count": "<n>"} envelope
// QueryPECCount's caller expects.
func countEnvelope(t *testing.T, count int) []byte {
	t.Helper()
	env := map[string]any{
		"data": map[string]any{
			"head": map[string]any{"vars": []string{"count"}},
			"results": map[string]any{
				"bindings": []map[string]any{
					{"count": map[string]string{"type": "literal", "value": strconv.Itoa(count)}},
				},
			},
		},
		"responseComplete": true,
		"id":               "x",
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return b
}

// routedMockCIMRequester discriminates between the discovery
// count query and the three device-enumeration queries by inspecting
// the request body for the count template's distinctive
// "COUNT(DISTINCT" fragment, and returns a different canned envelope for
// each. This is required (unlike mockCIMRequester's single canned
// response) so bootstrapRegistry's discover-vs-project comparison can
// be driven with independently controllable discovered/projected
// counts.
type routedMockCIMRequester struct {
	countResp  []byte
	deviceResp []byte
}

func (m *routedMockCIMRequester) Request(_ context.Context, _ string, body []byte) ([]byte, error) {
	src := m.deviceResp
	if bytes.Contains(body, []byte("COUNT(DISTINCT")) {
		src = m.countResp
	}
	out := make([]byte, len(src))
	copy(out, src)
	return out, nil
}

// TestBootstrapRegistryEmptyFleetFailsLoud is the core
// assertion: a feeder that returns zero PowerElectronicsConnection rows
// from every enumeration query (the load-modeled-feeder symptom) must
// make bootstrapRegistry return a non-nil error naming the feeder mRID
// that was queried and stating that zero PowerElectronicsConnection
// objects were found, with a hint that a load-modeled
// (EnergyConsumer-modeled) feeder is the likely cause. This error flows
// unchanged through run() to main()'s existing log.Fatalf convention;
// no separate os.Exit call is introduced here.
func TestBootstrapRegistryEmptyFleetFailsLoud(t *testing.T) {
	certDir := t.TempDir()
	requester := &mockCIMRequester{resp: emptyEnvelope(t)}
	client := cim.NewClient(requester)

	const feederMRID = "_DEADBEEF-0000-0000-0000-000000000001"
	reg, err := bootstrapRegistry(context.Background(), client, feederMRID, certDir, sep2embed.DeviceCertModeDevMint, nil, nil)
	if err == nil {
		t.Fatal("bootstrapRegistry with zero PowerElectronicsConnection rows: want error, got nil")
	}
	if reg != nil {
		t.Errorf("bootstrapRegistry returned a non-nil registry alongside the error: %+v", reg)
	}

	msg := err.Error()
	if !strings.Contains(msg, feederMRID) {
		t.Errorf("error message does not name the queried feeder mRID %q: %q", feederMRID, msg)
	}
	if !strings.Contains(msg, "zero") && !strings.Contains(msg, "0 ") {
		t.Errorf("error message does not state zero PowerElectronicsConnection objects were found: %q", msg)
	}
	if !strings.Contains(msg, "PowerElectronicsConnection") {
		t.Errorf("error message does not mention PowerElectronicsConnection: %q", msg)
	}
	if !strings.Contains(msg, "EnergyConsumer") {
		t.Errorf("error message does not hint at a load-modeled (EnergyConsumer) feeder as the likely cause: %q", msg)
	}
}

// TestBootstrapRegistryHappyPathNoSpuriousWarning confirms the
// discover-vs-project diagnostics add no new noise on a normal boot: a feeder whose
// discovered PEC count matches its fully-projected device count must
// boot with no error and must not log a WARNING-level line.
func TestBootstrapRegistryHappyPathNoSpuriousWarning(t *testing.T) {
	certDir := t.TempDir()
	requester := &routedMockCIMRequester{
		deviceResp: threeDeviceEnvelope(t),
		countResp:  countEnvelope(t, 3), // matches the 3 distinct mRIDs threeDeviceEnvelope dedupes to
	}
	client := cim.NewClient(requester)

	buf := captureLog(t)

	reg, err := bootstrapRegistry(context.Background(), client, "_DEADBEEF-0000-0000-0000-000000000123", certDir, sep2embed.DeviceCertModeDevMint, nil, nil)
	if err != nil {
		t.Fatalf("bootstrapRegistry: %v", err)
	}
	if got := reg.Len(); got != 3 {
		t.Fatalf("registry.Len() = %d, want 3", got)
	}

	logged := buf.String()
	// "bridge: WARNING" is this diagnostic's own prefix (see
	// pecCountLogLine's callers in bootstrapRegistry); sep2embed logs its
	// own unrelated "sep2embed: WARNING" lines on every dev-mint run
	// (expected, pre-existing behavior), so the assertion is scoped to
	// this package's diagnostic rather than any "WARNING" substring.
	if strings.Contains(logged, "bridge: WARNING") {
		t.Errorf("happy path (discovered == projected, non-empty fleet) logged a bridge WARNING; want none:\n%s", logged)
	}
	if !strings.Contains(logged, "no drops") {
		t.Errorf("happy path log output does not confirm the discover-vs-project match:\n%s", logged)
	}
}

// TestBootstrapRegistryLogsDropWhenDiscoveredExceedsProjected is the
// Cyrus INNER-join-drop-visibility assertion: when the discovery count
// query reports more PECs than were fully projected as devices, the log
// must surface both counts and flag them as a WARNING, distinguishing
// this case from the empty-fleet guard (the fleet here is non-empty; it
// is just short of what the feeder actually contains).
func TestBootstrapRegistryLogsDropWhenDiscoveredExceedsProjected(t *testing.T) {
	certDir := t.TempDir()
	requester := &routedMockCIMRequester{
		deviceResp: threeDeviceEnvelope(t), // 3 distinct mRIDs projected
		countResp:  countEnvelope(t, 7),    // feeder actually has 7 PECs; 4 dropped by the INNER joins
	}
	client := cim.NewClient(requester)

	buf := captureLog(t)

	reg, err := bootstrapRegistry(context.Background(), client, "_DEADBEEF-0000-0000-0000-000000000123", certDir, sep2embed.DeviceCertModeDevMint, nil, nil)
	if err != nil {
		t.Fatalf("bootstrapRegistry: %v", err)
	}
	if got := reg.Len(); got != 3 {
		t.Fatalf("registry.Len() = %d, want 3", got)
	}

	logged := buf.String()
	if !strings.Contains(logged, "bridge: WARNING") {
		t.Errorf("discovered(7) != projected(3): want a bridge WARNING-level log line, got none:\n%s", logged)
	}
	if !strings.Contains(logged, "7") || !strings.Contains(logged, "3") {
		t.Errorf("drop-visibility log does not surface both the discovered (7) and projected (3) counts:\n%s", logged)
	}
}

// TestParsePECCount pins the count-binding extraction contract:
// present-and-parsable => (n, true); missing binding, no rows, or an
// unparsable value => (0, false), never a fabricated non-zero number.
func TestParsePECCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		res     *cim.QueryDataResult
		wantOK  bool
		wantVal int
	}{
		{
			name: "present",
			res: &cim.QueryDataResult{
				Results: cim.SPARQLResults{Bindings: []map[string]cim.Binding{
					{"count": {Value: "14"}},
				}},
			},
			wantOK:  true,
			wantVal: 14,
		},
		{
			name: "zero-is-valid",
			res: &cim.QueryDataResult{
				Results: cim.SPARQLResults{Bindings: []map[string]cim.Binding{
					{"count": {Value: "0"}},
				}},
			},
			wantOK:  true,
			wantVal: 0,
		},
		{
			name:   "nil-result",
			res:    nil,
			wantOK: false,
		},
		{
			name: "no-rows",
			res: &cim.QueryDataResult{
				Results: cim.SPARQLResults{Bindings: []map[string]cim.Binding{}},
			},
			wantOK: false,
		},
		{
			name: "missing-binding",
			res: &cim.QueryDataResult{
				Results: cim.SPARQLResults{Bindings: []map[string]cim.Binding{
					{"other": {Value: "x"}},
				}},
			},
			wantOK: false,
		},
		{
			name: "unparsable",
			res: &cim.QueryDataResult{
				Results: cim.SPARQLResults{Bindings: []map[string]cim.Binding{
					{"count": {Value: "not-a-number"}},
				}},
			},
			wantOK: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := parsePECCount(tc.res)
			if ok != tc.wantOK {
				t.Fatalf("parsePECCount(%+v): ok = %v, want %v", tc.res, ok, tc.wantOK)
			}
			if ok && got != tc.wantVal {
				t.Errorf("parsePECCount(%+v) = %d, want %d", tc.res, got, tc.wantVal)
			}
		})
	}
}

// TestPECCountLogLine pins the discover-vs-project message content and
// its warn/info classification in isolation from bootstrapRegistry's
// wiring, matching the pure-function assertion style already used by
// TestDeviceCertModeMapping in this package.
func TestPECCountLogLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		discovered   int
		discoveredOK bool
		projected    int
		wantWarn     bool
		wantSubs     []string
		rejectSubs   []string
	}{
		{
			name:         "match-no-drops",
			discovered:   3,
			discoveredOK: true,
			projected:    3,
			wantWarn:     false,
			wantSubs:     []string{"3", "no drops"},
		},
		{
			name:         "mismatch-drops",
			discovered:   7,
			discoveredOK: true,
			projected:    3,
			wantWarn:     true,
			wantSubs:     []string{"7", "3", "4 dropped"},
		},
		{
			name:         "count-unavailable",
			discoveredOK: false,
			projected:    3,
			wantWarn:     true,
			wantSubs:     []string{"could not determine", "3"},
			rejectSubs:   []string{"0 dropped"},
		},
		{
			// This supersedes the earlier "clamp discovered <
			// projected to no-drops" assumption: that assumption was
			// exactly the blind spot that let a 9-PEC feeder mint 18
			// devices with a clean "no drops" log line. discovered <
			// projected now means some PowerElectronicsConnection
			// surfaced under more than one identity (a child
			// PowerElectronicsUnit bound under a different mRID than its
			// own parent PEC), and must be surfaced as a WARNING, not
			// silently folded into the counts-match path.
			name:         "discovered-less-than-projected-over-projection-warns",
			discovered:   9,
			discoveredOK: true,
			projected:    18,
			wantWarn:     true,
			wantSubs:     []string{"9", "18", "over-projected"},
			rejectSubs:   []string{"no drops", "dropped for missing"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			msg, warn := pecCountLogLine("_DEADBEEF-0000-0000-0000-000000000123", tc.discovered, tc.discoveredOK, tc.projected)
			if warn != tc.wantWarn {
				t.Errorf("pecCountLogLine(...): warn = %v, want %v (msg=%q)", warn, tc.wantWarn, msg)
			}
			for _, want := range tc.wantSubs {
				if !strings.Contains(msg, want) {
					t.Errorf("pecCountLogLine(...) = %q, want substring %q", msg, want)
				}
			}
			for _, reject := range tc.rejectSubs {
				if strings.Contains(msg, reject) {
					t.Errorf("pecCountLogLine(...) = %q, must not contain %q", msg, reject)
				}
			}
		})
	}
}
