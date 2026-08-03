package sep2embed

import (
	"reflect"
	"strings"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// TestDERControlModeTableCoversEveryOpModField is the drift guard on
// derControlModes.
//
// The table is the operative definition of "the DERControl controls" that
// 2018 Annex B p.160 scopes supersession to. A control mode missing from it is
// invisible to classifyModes, and an invisible mode reads as "nothing in
// common", which downgrades a genuine supersession to rule t) independence and
// leaves a device running two conflicting setpoints with both events marked
// Active. Nothing else in the suite would fail.
//
// The check is by reflection rather than by a hand-maintained count because
// the risk is a field added to core's DERControlBase later, which no
// hand-written number notices.
//
// rampTms is the one deliberate exclusion: it modifies whatever mode is
// present rather than being a mode, so two events differing only in rampTms
// are not "differing controls" in rule t)'s sense. It is named explicitly
// here so that excluding it stays a decision rather than an omission.
func TestDERControlModeTableCoversEveryOpModField(t *testing.T) {
	t.Parallel()

	listed := make(map[string]bool, len(derControlModes))
	for _, m := range derControlModes {
		if listed[m.name] {
			t.Errorf("derControlModes lists %q twice", m.name)
		}
		listed[m.name] = true
	}

	typ := reflect.TypeFor[sep2.DERControlBase]()
	seen := 0
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !strings.HasPrefix(f.Name, "OpMod") {
			if f.Name != "RampTms" {
				t.Errorf("sep2.DERControlBase gained field %q, which is neither an OpMod* control mode nor the known rampTms exclusion; decide whether it belongs in derControlModes", f.Name)
			}
			continue
		}
		// The wire name is the xml tag's first component, not the Go field
		// name: the two differ in case and the table carries the wire name.
		wire, _, _ := strings.Cut(f.Tag.Get("xml"), ",")
		if wire == "" {
			t.Fatalf("field %q carries no xml tag", f.Name)
		}
		if !listed[wire] {
			t.Errorf("sep2.DERControlBase field %q (%s) is missing from derControlModes; an unlisted control mode is invisible to supersession classification and silently degrades a same-set supersession to rule t) independence", f.Name, wire)
		}
		seen++
	}

	if seen != len(derControlModes) {
		t.Errorf("derControlModes lists %d modes, sep2.DERControlBase declares %d OpMod* fields", len(derControlModes), seen)
	}
}

// TestControlModesOfReportsExactlyWhatIsSet pins the mode extraction itself,
// including the two cases the classifier's correctness rests on: a nil base
// and a base with several modes.
func TestControlModesOfReportsExactlyWhatIsSet(t *testing.T) {
	t.Parallel()

	connect := true
	watt := &sep2.ActivePower{Multiplier: 0, Value: 5000}
	vars := &sep2.ReactivePower{Multiplier: 0, Value: 250}
	ramp := uint16(30)

	tests := []struct {
		name string
		base *sep2.DERControlBase
		want []string
	}{
		{"nil base", nil, nil},
		{"empty base", &sep2.DERControlBase{}, []string{}},
		{"one mode", &sep2.DERControlBase{OpModTargetW: watt}, []string{"opModTargetW"}},
		{
			"several modes, reported in schema order",
			&sep2.DERControlBase{OpModConnect: &connect, OpModTargetVar: vars, OpModTargetW: watt},
			[]string{"opModConnect", "opModTargetVar", "opModTargetW"},
		},
		{
			// rampTms is not a control mode: an event carrying only a ramp
			// time commands nothing and supersedes nothing.
			"rampTms alone is not a mode",
			&sep2.DERControlBase{RampTms: &ramp},
			[]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := controlModesOf(tt.base)
			if len(got) != len(tt.want) {
				t.Fatalf("controlModesOf = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("controlModesOf = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// TestClassifyModes covers the three-way split that decides whether an
// already-served event is marked Superseded, flagged potentiallySuperseded,
// or left alone.
func TestClassifyModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		existing, incoming []string
		want               modeRelation
		why                string
	}{
		{
			name: "same single mode", existing: []string{"opModTargetW"}, incoming: []string{"opModTargetW"},
			want: modesIdentical,
			why:  "2018 Annex B p.160 scopes EventStatus 4 to the exact same set of controls, which this is",
		},
		{
			name: "different single modes", existing: []string{"opModTargetW"}, incoming: []string{"opModTargetVar"},
			want: modesIndependent,
			why:  "2018 rule t) p.91: differing controls overlap without superseding",
		},
		{
			name: "same set, different order", existing: []string{"opModTargetW", "opModTargetVar"}, incoming: []string{"opModTargetVar", "opModTargetW"},
			want: modesIdentical,
			why:  "the control set is a set; ordering is not part of it",
		},
		{
			name: "incoming is a strict superset", existing: []string{"opModTargetW"}, incoming: []string{"opModTargetW", "opModTargetVar"},
			want: modesPartial,
			why:  "the earlier event's controls are all covered but the sets are not equal, which 2018 rule t)4) p.92 routes to the flag",
		},
		{
			name: "incoming is a strict subset", existing: []string{"opModTargetW", "opModTargetVar"}, incoming: []string{"opModTargetW"},
			want: modesPartial,
			why:  "overlap in SOME, BUT NOT ALL controls (2018 Annex B p.160)",
		},
		{
			name: "overlapping but neither contains the other", existing: []string{"opModTargetW", "opModConnect"}, incoming: []string{"opModTargetW", "opModEnergize"},
			want: modesPartial,
		},
		{
			name: "both empty", existing: nil, incoming: nil,
			want: modesIndependent,
			why:  "an event commanding no control mode supersedes nothing; an empty intersection is not a shared control set",
		},
		{
			name: "existing empty, incoming set", existing: nil, incoming: []string{"opModTargetW"},
			want: modesIndependent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyModes(tt.existing, tt.incoming); got != tt.want {
				t.Errorf("classifyModes(%v, %v) = %v, want %v; %s", tt.existing, tt.incoming, got, tt.want, tt.why)
			}
		})
	}
}

// TestIntervalsOverlap pins the temporal half of the supersession test,
// including the boundary case where one window ends exactly as the next
// begins.
func TestIntervalsOverlap(t *testing.T) {
	t.Parallel()

	iv := func(start int64, dur uint32) *sep2.DateTimeInterval {
		return &sep2.DateTimeInterval{Start: start, Duration: dur}
	}

	tests := []struct {
		name string
		a, b *sep2.DateTimeInterval
		want bool
		why  string
	}{
		{"identical windows", iv(1000, 100), iv(1000, 100), true, ""},
		{"partial overlap", iv(1000, 100), iv(1050, 100), true, ""},
		{"nested", iv(1000, 100), iv(1020, 10), true, "2018 rule f) p.90 covers Nested as well as Overlapping Events"},
		{
			"back to back", iv(1000, 100), iv(1100, 100), false,
			"the window is half-open: an event ending exactly when the next starts is not in force at the same instant, so it is not superseded and must run out its own window",
		},
		{"fully disjoint", iv(1000, 100), iv(5000, 100), false, ""},
		{
			"nil first", nil, iv(1000, 100), false,
			"an event whose temporal extent is unknown is not one this server edits; interval is minOccurs=1 so a nil is a record no path here wrote",
		},
		{"nil second", iv(1000, 100), nil, false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := intervalsOverlap(tt.a, tt.b); got != tt.want {
				t.Errorf("intervalsOverlap = %v, want %v; %s", got, tt.want, tt.why)
			}
			// The relation is symmetric; asserting it here means no caller
			// has to reason about argument order.
			if got := intervalsOverlap(tt.b, tt.a); got != tt.want {
				t.Errorf("intervalsOverlap (arguments reversed) = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSupersedesRequiresAStrictlyLaterCreationTime covers the rule f) half of
// the classification, which is the half that decides a tie.
//
// Equal creationTimes must NOT supersede in either direction. The client side
// this drives compares with a strict greater-than (the EPRI reference
// client's block_supersede evaluates `x->creationTime > y->creationTime`), so
// a server that treated equal as newer would mark an event superseded that
// the client is still running.
func TestSupersedesRequiresAStrictlyLaterCreationTime(t *testing.T) {
	t.Parallel()

	control := func(at int64, mode string) sep2.DERControl {
		var base sep2.DERControlBase
		switch mode {
		case "opModTargetW":
			base.OpModTargetW = &sep2.ActivePower{Value: 1}
		case "opModTargetVar":
			base.OpModTargetVar = &sep2.ReactivePower{Value: 1}
		}
		c := sep2.DERControl{DERControlBase: &base}
		c.CreationTime = at
		c.Interval = &sep2.DateTimeInterval{Start: at, Duration: 1800}
		return c
	}

	existing := control(1000, "opModTargetW")

	tests := []struct {
		name     string
		incoming sep2.DERControl
		want     modeRelation
		why      string
	}{
		{"later, same mode", control(1600, "opModTargetW"), modesIdentical, ""},
		{
			"same instant, same mode", control(1000, "opModTargetW"), modesIndependent,
			"neither is newer under rule f) p.90, and a strict-greater client comparison would run neither; nextEventCreationTime exists so the write path never produces this",
		},
		{"earlier, same mode", control(500, "opModTargetW"), modesIndependent, "the older event does not supersede the newer one"},
		{"later, different mode", control(1600, "opModTargetVar"), modesIndependent, "2018 rule t) p.91"},
		{
			"later, same mode, non-overlapping window",
			func() sep2.DERControl {
				c := control(1600, "opModTargetW")
				c.Interval = &sep2.DateTimeInterval{Start: 9000, Duration: 100}
				return c
			}(),
			modesIndependent,
			"supersession requires overlap in time as well as in controls (2018 Annex B p.160)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := supersedes(existing, tt.incoming); got != tt.want {
				t.Errorf("supersedes = %v, want %v; %s", got, tt.want, tt.why)
			}
		})
	}
}

// TestEventStatusPerEdition is the direct statement of the edition split, and
// it is the reason both branches are written rather than only the one this
// server currently selects.
//
// 2018 and 2023 disagree on every value in this table. Testing only the
// selected branch would leave the 2023 branch as untested prose, and the
// point of making the edition an explicit parameter is that a future 2023
// mode is a configuration change rather than a rewrite.
func TestEventStatusPerEdition(t *testing.T) {
	t.Parallel()

	const created = int64(1000)
	const supersededAt = int64(1600)

	t.Run("2018 fresh event", func(t *testing.T) {
		got := newEventStatus(edition2018, created)
		if got.CurrentStatus != sep2.EventStatusActive || got.DateTime != created {
			t.Errorf("newEventStatus(2018) = %+v, want Active at %d", got, created)
		}
		if got.PotentiallySuperseded {
			t.Error("newEventStatus(2018).PotentiallySuperseded = true, want false; under 2018 the flag means PARTIAL supersession (Annex B p.160) and a fresh event has nothing to be partly superseded by")
		}
		if got.PotentiallySupersededTime != nil {
			t.Error("newEventStatus(2018).PotentiallySupersededTime is set with the flag false; it records the instant the flag was set (2018 p.161)")
		}
	})

	t.Run("2023 fresh event", func(t *testing.T) {
		got := newEventStatus(edition2023, created)
		if got.CurrentStatus != sep2.EventStatusActive {
			t.Errorf("newEventStatus(2023).CurrentStatus = %d, want %d", got.CurrentStatus, sep2.EventStatusActive)
		}
		if !got.PotentiallySuperseded {
			t.Error("newEventStatus(2023).PotentiallySuperseded = false, want true; 2023 Annex B p.169 deprecates the element to \"SHALL be set to true\"")
		}
		if got.PotentiallySupersededTime != nil {
			t.Error("newEventStatus(2023).PotentiallySupersededTime is set; 2023 p.170 says it SHALL NOT be included by servers")
		}
	})

	t.Run("2018 marks superseded", func(t *testing.T) {
		es := newEventStatus(edition2018, created)
		if !markSuperseded(edition2018, es, supersededAt) {
			t.Fatal("markSuperseded(2018) reported no change on an Active event")
		}
		if es.CurrentStatus != sep2.EventStatusSuperseded {
			t.Errorf("currentStatus = %d, want %d (Superseded); 2018 Annex B p.160 makes this a server SHALL for a same-program, same-control-set, overlapping replacement",
				es.CurrentStatus, sep2.EventStatusSuperseded)
		}
		if es.DateTime != supersededAt {
			t.Errorf("dateTime = %d, want %d (the superseding event's Effective Start Time)", es.DateTime, supersededAt)
		}
		if es.PotentiallySuperseded {
			t.Error("full supersession set potentiallySuperseded; that flag carries PARTIAL supersession and setting it here tells a client the event is still partly in force")
		}

		// Set-once: 2018 Annex B p.160 says the EARLIEST Effective Start Time
		// of the overlapping event, so a third, later control must not push
		// the recorded instant forward.
		if markSuperseded(edition2018, es, supersededAt+5000) {
			t.Error("markSuperseded restamped an already-superseded event")
		}
		if es.DateTime != supersededAt {
			t.Errorf("dateTime = %d after a second supersession, want %d (the EARLIEST overlapping start)", es.DateTime, supersededAt)
		}
	})

	t.Run("2023 does not mark superseded", func(t *testing.T) {
		es := newEventStatus(edition2023, created)
		if markSuperseded(edition2023, es, supersededAt) {
			t.Error("markSuperseded(2023) reported a change")
		}
		if es.CurrentStatus != sep2.EventStatusActive {
			t.Errorf("currentStatus = %d, want %d; 2023 Annex B p.169 deprecates value 4 as \"SHALL NOT be used by servers\" and rewords value 1 to mean active even when known to be overlapped",
				es.CurrentStatus, sep2.EventStatusActive)
		}
		if es.DateTime != created {
			t.Errorf("dateTime = %d, want %d unchanged", es.DateTime, created)
		}
	})

	t.Run("2018 flags partial supersession", func(t *testing.T) {
		es := newEventStatus(edition2018, created)
		if !markPotentiallySuperseded(edition2018, es, supersededAt) {
			t.Fatal("markPotentiallySuperseded(2018) reported no change")
		}
		if !es.PotentiallySuperseded {
			t.Error("potentiallySuperseded = false; 2018 rules q)3) p.91 and t)4) p.92 make setting it a server SHALL")
		}
		if es.PotentiallySupersededTime == nil || *es.PotentiallySupersededTime != supersededAt {
			t.Errorf("potentiallySupersededTime = %v, want %d; the same rules require it to be updated alongside the flag", es.PotentiallySupersededTime, supersededAt)
		}
		if es.CurrentStatus != sep2.EventStatusActive {
			t.Errorf("currentStatus = %d, want %d; a partly superseded event is still partly in force, which is exactly why the standard uses a flag here rather than status 4",
				es.CurrentStatus, sep2.EventStatusActive)
		}

		// Set-once, for the same reason as markSuperseded: the field records
		// when the flag was set (2018 p.161), not when it was last confirmed.
		if markPotentiallySuperseded(edition2018, es, supersededAt+5000) {
			t.Error("markPotentiallySuperseded restamped an already-flagged event")
		}
		if *es.PotentiallySupersededTime != supersededAt {
			t.Errorf("potentiallySupersededTime = %d after a second partial overlap, want %d", *es.PotentiallySupersededTime, supersededAt)
		}
	})

	t.Run("2023 does not flag partial supersession", func(t *testing.T) {
		es := newEventStatus(edition2023, created)
		if markPotentiallySuperseded(edition2023, es, supersededAt) {
			t.Error("markPotentiallySuperseded(2023) reported a change")
		}
		if es.PotentiallySupersededTime != nil {
			t.Error("potentiallySupersededTime is set under 2023; p.170 says it SHALL NOT be included by servers")
		}
	})

	t.Run("nil status is a no-op, not a panic", func(t *testing.T) {
		if markSuperseded(edition2018, nil, supersededAt) || markPotentiallySuperseded(edition2018, nil, supersededAt) {
			t.Error("marking a nil EventStatus reported a change")
		}
	})
}

// TestServedEventEditionIsPinnedTo2018 states the selection explicitly.
//
// Every behavioral branch above is reachable through the eventEdition
// argument, so this constant is the whole of what a future 2023 mode has to
// move. Pinning it in a test means a change of edition is a deliberate edit
// with a failing test attached rather than a silent shift in what every
// device is served.
func TestServedEventEditionIsPinnedTo2018(t *testing.T) {
	t.Parallel()

	if servedEventEdition != edition2018 {
		t.Fatal("servedEventEdition changed away from edition2018; the EPRI reference client this bridge is verified against implements 2018, and the change alters the EventStatus every device is served. Update this test in the same commit and state why")
	}
}

// TestNextEventCreationTimeBreaksSameSecondTies covers the write path's clock
// guard.
//
// creationTime is the only discriminator between two overlapping controls of
// equal primacy (2018 rule f) p.90), TimeType has one-second resolution
// (sep.xsd:6382), and the client comparison is strictly greater. Two deltas
// for one device inside one second would otherwise produce two controls
// neither of which supersedes the other, and the client would keep running
// the older one: this card's defect arriving through the clock instead of
// through the mRID.
func TestNextEventCreationTimeBreaksSameSecondTies(t *testing.T) {
	t.Parallel()

	wattBase := sep2.DERControlBase{OpModTargetW: &sep2.ActivePower{Value: 1}}
	varBase := sep2.DERControlBase{OpModTargetVar: &sep2.ReactivePower{Value: 1}}

	issued := func(at int64, base sep2.DERControlBase) sep2.DERControl {
		c := sep2.DERControl{DERControlBase: &base}
		c.CreationTime = at
		c.Interval = &sep2.DateTimeInterval{Start: at, Duration: 1800}
		return c
	}

	tests := []struct {
		name  string
		prior []sep2.DERControl
		base  sep2.DERControlBase
		wall  int64
		want  int64
		why   string
	}{
		{
			name: "no prior controls", prior: nil, base: wattBase, wall: 1000, want: 1000,
			why: "the wall clock is used as is when nothing constrains it",
		},
		{
			name:  "prior control on the same mode in the same second",
			prior: []sep2.DERControl{issued(1000, wattBase)}, base: wattBase, wall: 1000, want: 1001,
			why: "equal creationTimes compare false in both directions under a strict-greater client comparison, so the incoming control would be discarded",
		},
		{
			name:  "prior control on the same mode in a later second",
			prior: []sep2.DERControl{issued(1500, wattBase)}, base: wattBase, wall: 1000, want: 1501,
			why: "a clock that went backwards must not produce a control the client ranks as older than one it already holds",
		},
		{
			name:  "prior control on the same mode, already older",
			prior: []sep2.DERControl{issued(500, wattBase)}, base: wattBase, wall: 1000, want: 1000,
			why: "the wall clock already ranks the incoming control newer; there is nothing to break",
		},
		{
			name:  "prior control on an INDEPENDENT mode in the same second",
			prior: []sep2.DERControl{issued(1000, varBase)}, base: wattBase, wall: 1000, want: 1000,
			why: "2018 rule t) p.91 makes differing controls independent, so no ordering between them is ever evaluated and sharing a second is harmless",
		},
		{
			name: "several prior controls, the latest same-mode one wins",
			prior: []sep2.DERControl{
				issued(1000, wattBase),
				issued(1002, wattBase),
				issued(5000, varBase),
			},
			base: wattBase, wall: 1000, want: 1003,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextEventCreationTime(tt.prior, tt.base, tt.wall); got != tt.want {
				t.Errorf("nextEventCreationTime = %d, want %d; %s", got, tt.want, tt.why)
			}
		})
	}
}
