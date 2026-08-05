package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/buildinfo"
)

// TestHandleVersionFlagPrintsVersionAndReportsHandled locks in the
// -version contract: when loadConfig's error is
// errVersionRequested, handleVersionFlag writes buildinfo.Version to
// its writer and reports true, and it does so as a pure function with
// no side effect beyond that write (no ctx, no listener bind, no CIM
// query, no bus connect: main only calls loadConfig, then this
// function, before any of run()'s side-effecting code runs). The
// printed value is asserted for its actual content, not just that
// something non-empty came out, per data-invariants.
func TestHandleVersionFlagPrintsVersionAndReportsHandled(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handled := handleVersionFlag(&buf, errVersionRequested)

	if !handled {
		t.Fatal("handleVersionFlag: got false, want true for errVersionRequested")
	}
	got := buf.String()
	if !strings.Contains(got, buildinfo.Version) {
		t.Errorf("handleVersionFlag output %q does not contain buildinfo.Version %q", got, buildinfo.Version)
	}
	if !strings.Contains(got, "bridge ") {
		t.Errorf("handleVersionFlag output %q does not contain the \"bridge \" prefix", got)
	}
}

// TestHandleVersionFlagIgnoresOtherErrors confirms handleVersionFlag
// only fires on errVersionRequested: an unrelated loadConfig error (a
// missing required field, say) must not be misreported as a version
// request, and must not write anything to w. Without this guard, main
// would print a bogus version line and exit 0 on a genuine config
// error instead of surfacing it via log.Fatalf.
func TestHandleVersionFlagIgnoresOtherErrors(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handled := handleVersionFlag(&buf, errors.New("config: SEP2_STOMP_ADDR / -stomp-addr is required"))

	if handled {
		t.Fatal("handleVersionFlag: got true, want false for a non-version error")
	}
	if buf.Len() != 0 {
		t.Errorf("handleVersionFlag: wrote %q to w, want no output for a non-version error", buf.String())
	}
}
