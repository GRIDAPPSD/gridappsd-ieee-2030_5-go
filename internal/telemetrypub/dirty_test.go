package telemetrypub

import (
	"sync"
	"testing"

	"github.com/GRIDAPPSD/gridappsd-ieee-2030_5-go/internal/sep2embed"
)

func mrids(devices []sep2embed.DERStatusSnapshot) []string {
	out := make([]string, 0, len(devices))
	for _, d := range devices {
		out = append(out, d.MRID)
	}
	return out
}

func assertMRIDs(t *testing.T, label string, got []sep2embed.DERStatusSnapshot, want ...string) {
	t.Helper()
	gotMRIDs := mrids(got)
	if len(gotMRIDs) != len(want) {
		t.Fatalf("%s selected %v, want %v", label, gotMRIDs, want)
	}
	for i := range want {
		if gotMRIDs[i] != want[i] {
			t.Fatalf("%s selected %v, want %v", label, gotMRIDs, want)
		}
	}
}

// TestDirtyTrackerSelectsEveryDeviceOnFirstSight: nothing has been
// published yet, so every device is new information.
func TestDirtyTrackerSelectsEveryDeviceOnFirstSight(t *testing.T) {
	t.Parallel()

	d := NewDirtyTracker(DiffFingerprint)
	devices := []sep2embed.DERStatusSnapshot{snapWithMode("mrid-a", 1), snapWithMode("mrid-b", 2)}

	assertMRIDs(t, "first Select", d.Select(devices).Devices, "mrid-a", "mrid-b")
}

// TestDirtyTrackerOmitsUnchangedDevicesAfterPublish is the core
// suppression contract: once a device's values have been published, an
// interval where nothing about them moved selects nothing for it.
func TestDirtyTrackerOmitsUnchangedDevicesAfterPublish(t *testing.T) {
	t.Parallel()

	d := NewDirtyTracker(DiffFingerprint)
	devices := []sep2embed.DERStatusSnapshot{snapWithMode("mrid-a", 1), snapWithMode("mrid-b", 2)}

	d.Published(d.Select(devices))

	if got := d.Select(devices).Devices; len(got) != 0 {
		t.Fatalf("second Select on unchanged devices returned %v, want none", mrids(got))
	}

	// Move exactly one device's mapped value: only that device returns.
	changed := []sep2embed.DERStatusSnapshot{snapWithMode("mrid-a", 1), snapWithMode("mrid-b", 5)}
	assertMRIDs(t, "Select after mrid-b changed", d.Select(changed).Devices, "mrid-b")
}

// TestDirtyTrackerKeepsDevicesDirtyUntilPublishSucceeds is the data-loss
// guard Craig called out: a batch that was built but never successfully
// sent must not clear anything, so the next interval re-selects it. A
// tracker that cleared at Select time would drop the update permanently
// the moment the bus hiccupped.
func TestDirtyTrackerKeepsDevicesDirtyUntilPublishSucceeds(t *testing.T) {
	t.Parallel()

	d := NewDirtyTracker(DiffFingerprint)
	devices := []sep2embed.DERStatusSnapshot{snapWithMode("mrid-a", 1)}

	first := d.Select(devices)
	assertMRIDs(t, "first Select", first.Devices, "mrid-a")

	// Publish failed: Published is never called.
	assertMRIDs(t, "Select after a failed publish", d.Select(devices).Devices, "mrid-a")

	// Publish succeeded this time.
	d.Published(d.Select(devices))
	if got := d.Select(devices).Devices; len(got) != 0 {
		t.Fatalf("Select after a successful publish returned %v, want none", mrids(got))
	}
}

// TestDirtyTrackerMarkDuringPublishSurvivesTheClear is the generation
// guard. An arrival that lands after the batch was selected but before
// Published runs (which is exactly what the core-side arrival observer
// will do, on the 2030.5 request goroutine) must NOT be cleared by that
// Published call: the value it marked was never in the sent batch.
func TestDirtyTrackerMarkDuringPublishSurvivesTheClear(t *testing.T) {
	t.Parallel()

	d := NewDirtyTracker(DiffFingerprint)
	old := []sep2embed.DERStatusSnapshot{snapWithMode("mrid-a", 1)}

	batch := d.Select(old)
	assertMRIDs(t, "first Select", batch.Devices, "mrid-a")

	// A new status arrives mid-publish and marks the device.
	d.Mark("mrid-a")

	// The in-flight publish then succeeds and reports the OLD batch.
	d.Published(batch)

	// The newer value must still be pending, even though the device's
	// stored values happen to be unchanged from what we just sent: the
	// mark is the signal, and it was never satisfied.
	assertMRIDs(t, "Select after a mid-publish arrival", d.Select(old).Devices, "mrid-a")
}

// TestDirtyTrackerMarkIsTheGate: the dirty set decides what gets
// published, not the value comparison. An explicit Mark selects the
// device even when its values are identical to what was last published,
// because whoever marks it is asserting there is something to say. This
// is the contract the core-side arrival observer will rely on: mark,
// and it goes out.
func TestDirtyTrackerMarkIsTheGate(t *testing.T) {
	t.Parallel()

	d := NewDirtyTracker(DiffFingerprint)
	devices := []sep2embed.DERStatusSnapshot{snapWithMode("mrid-a", 1)}
	d.Published(d.Select(devices))
	if got := d.Select(devices).Devices; len(got) != 0 {
		t.Fatalf("Select with no mark returned %v, want none", mrids(got))
	}

	d.Mark("mrid-a")
	assertMRIDs(t, "Select after an explicit mark", d.Select(devices).Devices, "mrid-a")
}

// TestDirtyTrackerRecordsWhatWasActuallySent: Published records the
// fingerprint of the values in the batch, not of whatever the tracker
// last saw. Otherwise a device that changed between Select and Published
// would be recorded as published at a value that never went out.
func TestDirtyTrackerRecordsWhatWasActuallySent(t *testing.T) {
	t.Parallel()

	d := NewDirtyTracker(DiffFingerprint)
	sent := []sep2embed.DERStatusSnapshot{snapWithMode("mrid-a", 1)}
	batch := d.Select(sent)
	d.Published(batch)

	// The device now reports a different value: it must be selected.
	newer := []sep2embed.DERStatusSnapshot{snapWithMode("mrid-a", 9)}
	assertMRIDs(t, "Select on a changed value", d.Select(newer).Devices, "mrid-a")
}

// TestAlwaysDirtySelectsEverythingEveryInterval pins the behaviour
// behind the suppression switch's OFF position: full-snapshot
// semantics, every device every interval, nothing suppressed.
func TestAlwaysDirtySelectsEverythingEveryInterval(t *testing.T) {
	t.Parallel()

	var d AlwaysDirty
	devices := []sep2embed.DERStatusSnapshot{snapWithMode("mrid-a", 1), snapWithMode("mrid-b", 2)}

	for i := range 3 {
		batch := d.Select(devices)
		assertMRIDs(t, "AlwaysDirty Select", batch.Devices, "mrid-a", "mrid-b")
		d.Published(batch)
		if i == 0 {
			d.Mark("mrid-a") // no-op, must not panic or change anything
		}
	}
}

// TestDirtyTrackerConcurrentMarkAndSelect exercises the exact race the
// core-side arrival observer will introduce: Mark called from protocol
// request goroutines while the publisher's goroutine selects and clears.
// The assertion is the race detector plus a coherent result.
func TestDirtyTrackerConcurrentMarkAndSelect(t *testing.T) {
	t.Parallel()

	d := NewDirtyTracker(DiffFingerprint)
	devices := []sep2embed.DERStatusSnapshot{snapWithMode("mrid-a", 1), snapWithMode("mrid-b", 2)}

	var wg sync.WaitGroup
	for _, id := range []string{"mrid-a", "mrid-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				d.Mark(id)
			}
		}()
	}

	for range 500 {
		batch := d.Select(devices)
		for _, dev := range batch.Devices {
			if dev.MRID != "mrid-a" && dev.MRID != "mrid-b" {
				t.Errorf("selected unknown device %q", dev.MRID)
			}
		}
		d.Published(batch)
	}
	wg.Wait()
}
