package sep2acl

import (
	"net/http"
	"testing"
)

func TestMethodAllowedFamilyMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		path        string
		method      string
		wantAllowed bool
		wantMatched bool
	}{
		// Read-only, spec-fixed families.
		{"dcap GET allowed", "/dcap", http.MethodGet, true, true},
		{"dcap HEAD allowed", "/dcap", http.MethodHead, true, true},
		{"dcap POST denied", "/dcap", http.MethodPost, false, true},
		{"dcap PUT denied", "/dcap", http.MethodPut, false, true},
		{"tm POST denied", "/tm", http.MethodPost, false, true},
		{"sdev GET allowed", "/sdev", http.MethodGet, true, true},
		{"sdev/sdi GET allowed (prefix)", "/sdev/sdi", http.MethodGet, true, true},
		{"sdev POST denied", "/sdev", http.MethodPost, false, true},
		{"dc GET allowed", "/dc", http.MethodGet, true, true},
		{"dc PUT denied", "/dc", http.MethodPut, false, true},
		{"rt GET allowed", "/rt", http.MethodGet, true, true},
		{"rt/{id} GET allowed (prefix)", "/rt/abc123", http.MethodGet, true, true},
		{"rt POST denied", "/rt", http.MethodPost, false, true},

		// Common/global families: create allowed at the family root.
		{"edev list GET allowed", "/edev", http.MethodGet, true, true},
		{"edev create POST allowed", "/edev", http.MethodPost, true, true},
		{"edev PUT denied", "/edev", http.MethodPut, false, true},
		{"mup GET allowed", "/mup", http.MethodGet, true, true},
		{"mup POST allowed", "/mup", http.MethodPost, true, true},
		{"mup DELETE denied", "/mup", http.MethodDelete, false, true},
		{"upt POST allowed", "/upt", http.MethodPost, true, true},
		{"msg GET allowed", "/msg", http.MethodGet, true, true},
		{"msg/{id}/tm POST allowed (prefix)", "/msg/1/tm", http.MethodPost, true, true},
		{"rsps GET allowed", "/rsps", http.MethodGet, true, true},

		// /edev/{id}-scoped families.
		{"edev/{id} GET allowed", "/edev/DEV-A", http.MethodGet, true, true},
		{"edev/{id} PUT allowed", "/edev/DEV-A", http.MethodPut, true, true},
		{"edev/{id} DELETE allowed", "/edev/DEV-A", http.MethodDelete, true, true},
		{"edev/{id} POST denied", "/edev/DEV-A", http.MethodPost, false, true},
		{"edev/{id}/rg GET allowed", "/edev/DEV-A/rg", http.MethodGet, true, true},
		{"edev/{id}/rg PUT denied", "/edev/DEV-A/rg", http.MethodPut, false, true},
		{"edev/{id}/sub GET allowed", "/edev/DEV-A/sub", http.MethodGet, true, true},
		{"edev/{id}/sub POST allowed", "/edev/DEV-A/sub", http.MethodPost, true, true},
		{"edev/{id}/sub/{subId} DELETE allowed (prefix)", "/edev/DEV-A/sub/1", http.MethodDelete, true, true},
		{"edev/{id}/der GET allowed", "/edev/DEV-A/der", http.MethodGet, true, true},
		{"edev/{id}/der PUT denied", "/edev/DEV-A/der", http.MethodPut, false, true},
		// DER instance (GAGO-111): the WADL declares PUTDER mode O, so the
		// instance path must be read-write, not the DER list's read-only
		// classification. Verifies neither the list (3 segments, above)
		// nor the specific writable sub-resources (5 segments, below) had
		// their own classification disturbed by adding this 4-segment
		// entry to the table.
		{"der/{derId} GET allowed", "/edev/DEV-A/der/1", http.MethodGet, true, true},
		{"der/{derId} HEAD allowed", "/edev/DEV-A/der/1", http.MethodHead, true, true},
		{"der/{derId} PUT allowed", "/edev/DEV-A/der/1", http.MethodPut, true, true},
		{"der/{derId} POST denied", "/edev/DEV-A/der/1", http.MethodPost, false, true},
		{"der/{derId} DELETE denied", "/edev/DEV-A/der/1", http.MethodDelete, false, true},
		{"der/{derId}/dercap PUT allowed", "/edev/DEV-A/der/1/dercap", http.MethodPut, true, true},
		{"der/{derId}/derg PUT allowed", "/edev/DEV-A/der/1/derg", http.MethodPut, true, true},
		{"der/{derId}/ders PUT allowed", "/edev/DEV-A/der/1/ders", http.MethodPut, true, true},
		{"der/{derId}/dera PUT allowed", "/edev/DEV-A/der/1/dera", http.MethodPut, true, true},
		{"der/{derId}/dercap DELETE denied", "/edev/DEV-A/der/1/dercap", http.MethodDelete, false, true},
		{"fsa GET allowed", "/edev/DEV-A/fsa", http.MethodGet, true, true},
		{"fsa/{fsaId} GET allowed (prefix)", "/edev/DEV-A/fsa/1", http.MethodGet, true, true},
		{"fsa/{fsaId}/derp GET allowed (prefix)", "/edev/DEV-A/fsa/1/derp", http.MethodGet, true, true},
		{"fsa/.../derc GET allowed (prefix)", "/edev/DEV-A/fsa/1/derp/2/derc", http.MethodGet, true, true},
		{"fsa/.../derc PUT denied (still RO fsa family)", "/edev/DEV-A/fsa/1/derp/2/derc", http.MethodPut, false, true},
		{"fsa/.../dderc GET allowed (more specific)", "/edev/DEV-A/fsa/1/derp/2/dderc", http.MethodGet, true, true},
		{"fsa/.../dderc PUT allowed (more specific overrides RO fsa)", "/edev/DEV-A/fsa/1/derp/2/dderc", http.MethodPut, true, true},
		{"fsa/.../dderc DELETE denied", "/edev/DEV-A/fsa/1/derp/2/dderc", http.MethodDelete, false, true},
		{"cfg PUT allowed", "/edev/DEV-A/cfg", http.MethodPut, true, true},
		{"dstat PUT allowed", "/edev/DEV-A/dstat", http.MethodPut, true, true},
		// LogEvent list, at the WADL address /edev/{id}/lel (GAGO-132).
		// Core v0.13.0 moved the list here from /edev/{id}/log, which was
		// never a WADL address and is no longer served, so the table must
		// classify the new address and no longer the old one.
		{"lel POST allowed", "/edev/DEV-A/lel", http.MethodPost, true, true},
		{"lel GET allowed", "/edev/DEV-A/lel", http.MethodGet, true, true},
		{"lel PUT denied", "/edev/DEV-A/lel", http.MethodPut, false, true},
		{"lel/{lelId} GET allowed (prefix)", "/edev/DEV-A/lel/1", http.MethodGet, true, true},
		// The retired /log address must fall through to the /edev/{id}
		// singleton entry rather than keep its own family: it matches as a
		// prefix (matched=true) but earns only that entry's GET/PUT/DELETE,
		// proving no dead readCreate rule survives for a path core no
		// longer serves.
		{"retired log POST no longer allowed", "/edev/DEV-A/log", http.MethodPost, false, true},
		{"ps PUT allowed", "/edev/DEV-A/ps", http.MethodPut, true, true},
		{"frq POST allowed", "/edev/DEV-A/frq", http.MethodPost, true, true},
		{"frp GET allowed", "/edev/DEV-A/frp", http.MethodGet, true, true},
		{"frp POST denied", "/edev/DEV-A/frp", http.MethodPost, false, true},

		// Unmatched / unrecognized paths: no family matches at all.
		{"unrecognized top-level path", "/nope", http.MethodGet, false, false},
		{"root path", "/", http.MethodGet, false, false},
		{"empty path", "", http.MethodGet, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			allowed, matched := MethodAllowed(tt.path, tt.method)
			if allowed != tt.wantAllowed || matched != tt.wantMatched {
				t.Errorf("MethodAllowed(%q, %q) = (allowed=%v, matched=%v), want (allowed=%v, matched=%v)",
					tt.path, tt.method, allowed, matched, tt.wantAllowed, tt.wantMatched)
			}
		})
	}
}

func TestEndDeviceID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		path   string
		wantID string
		wantOK bool
	}{
		{"top-level edev list has no id", "/edev", "", false},
		{"top-level edev list with trailing slash has no id", "/edev/", "", false},
		{"edev singleton has id", "/edev/DEV-A", "DEV-A", true},
		{"edev singleton with trailing slash has id", "/edev/DEV-A/", "DEV-A", true},
		{"nested edev resource has id", "/edev/DEV-A/der/1/dercap", "DEV-A", true},
		{"non-edev family has no id", "/mup/DEV-A", "", false},
		{"root path has no id", "/", "", false},
		{"empty path has no id", "", "", false},
		{
			// Boundary-of-the-boundary: a malformed path with a blank id
			// segment is still reported as edev-scoped (ok=true) with an
			// empty id, so the caller's ownership check runs against it
			// and denies, rather than the request skipping the check.
			"malformed double-slash id is edev-scoped with a blank id, not skipped",
			"/edev//x", "", true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			id, ok := EndDeviceID(tt.path)
			if id != tt.wantID || ok != tt.wantOK {
				t.Errorf("EndDeviceID(%q) = (%q, %v), want (%q, %v)", tt.path, id, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}
