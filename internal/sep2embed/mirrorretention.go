package sep2embed

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/assembly"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"
)

// Defaults for the mirror reading bounds. A device posting three readings
// every 30 s adds 8640 records a day, and nothing else removes them, so an
// unbounded store grows for the life of the process. Six hours is the span
// the telemetry history keeps, and 1440 per series is that history's sample
// cap, which also holds a reading every 15 s for six hours.
const (
	DefaultMirrorReadingRetention    = 6 * time.Hour
	DefaultMirrorReadingMaxPerSeries = 1440
	DefaultMirrorRetentionInterval   = time.Minute
)

// sweepMirrorReadings removes stored mirror readings older than maxAge before
// now, then the oldest of each series beyond maxPerSeries, and returns how
// many it removed. A series is one device and one reading type (see
// MirrorSeries), counted across the device's mirrors.
//
// When led is not nil and shows that nothing can be due (see sweepLedger), the
// store is not copied at all.
//
// A reading that cannot be deleted is logged and left for the next sweep; a
// failure to list the readings is returned and nothing is removed, since an
// incomplete listing would cap the wrong records.
func sweepMirrorReadings(ctx context.Context, stores *assembly.Stores, led *sweepLedger, now time.Time, maxAge time.Duration, maxPerSeries int) (int, error) {
	if maxAge <= 0 || maxPerSeries <= 0 {
		return 0, errors.New("sep2embed: mirror retention needs a positive age and per-series count")
	}
	cutoff := now.Add(-maxAge).Unix()
	if led != nil {
		due, err := led.due(ctx, stores, cutoff, maxPerSeries)
		if err != nil {
			return 0, err
		}
		if !due {
			return 0, nil
		}
		led.reset()
	}
	scan, err := collectMirrorRecords(ctx, stores, nil, 0)
	if err != nil {
		return 0, err
	}
	recs := scan.records

	drop := make([]bool, len(recs))
	live := map[seriesKey]int{}
	for i, r := range recs {
		if r.mmr.LastUpdateTime < cutoff {
			drop[i] = true
			continue
		}
		live[r.key]++
	}
	// recs is oldest first, so the first survivors of an over-cap series are
	// its oldest.
	for i, r := range recs {
		if !drop[i] && live[r.key] > maxPerSeries {
			drop[i] = true
			live[r.key]--
		}
	}
	keepTypedReading(recs, drop)

	removed, failed := 0, 0
	gone := map[string]int{}
	for i, r := range recs {
		if !drop[i] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		switch err := stores.MirrorMeterReadings.Delete(ctx, r.mirror, r.id); {
		case err == nil:
			removed++
			gone[r.mirror]++
		case isNotFound(err):
			gone[r.mirror]++
		default:
			failed++
			log.Printf("sep2embed: mirror retention: leaving reading %s/%s for the next sweep: %v", r.mirror, r.id, err)
		}
	}
	if removed > 0 || failed > 0 {
		log.Printf("sep2embed: mirror retention: removed=%d left=%d", removed, failed)
	}
	if led != nil && failed == 0 {
		led.record(scan, recs, drop, gone)
	}
	return removed, nil
}

// sweepLedger lets the sweep skip the full copy of the store when nothing can
// be due. It remembers, from the last full pass, how many readings each
// mirror stored and the largest series of each device. A sweep is skipped
// only when, for every mirror, the oldest stored reading is inside the
// retention age and the store has not shrunk behind the sweep's back, and
// for every device the largest series plus every reading added since cannot
// exceed the cap. That bound counts all of a device's new readings against
// one series, so it errs toward a full pass, never away from one.
//
// A ledger belongs to one goroutine; the zero value knows nothing and so
// always asks for a full pass.
type sweepLedger struct {
	mirrors map[string]scannedMirror
	peak    map[string]int
}

func (l *sweepLedger) reset() {
	l.mirrors, l.peak = nil, nil
}

// record stores what a full pass left behind.
func (l *sweepLedger) record(scan mirrorScan, recs []mirrorRecord, drop []bool, gone map[string]int) {
	l.mirrors = make(map[string]scannedMirror, len(scan.mirrors))
	for id, m := range scan.mirrors {
		l.mirrors[id] = scannedMirror{device: m.device, total: m.total - gone[id]}
	}
	live := map[seriesKey]int{}
	l.peak = map[string]int{}
	for i, r := range recs {
		if drop[i] {
			continue
		}
		live[r.key]++
		l.peak[r.key.device] = max(l.peak[r.key.device], live[r.key])
	}
}

// due reports whether a full pass is needed, and when it is not, advances
// the ledger by the readings added since the last pass.
func (l *sweepLedger) due(ctx context.Context, stores *assembly.Stores, cutoff int64, maxPerSeries int) (bool, error) {
	if l.mirrors == nil {
		return true, nil
	}
	if store.IsAbsent(stores.MirrorMeterReadings) || store.IsAbsent(stores.MirrorUsagePoints) {
		return false, nil
	}
	parents, err := stores.MirrorMeterReadings.Parents(ctx)
	if err != nil {
		return false, fmt.Errorf("sep2embed: mirror retention: list mirrors: %w", err)
	}
	next := make(map[string]scannedMirror, len(parents))
	bound := map[string]int{}
	for _, id := range parents {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n, err := stores.MirrorMeterReadings.Count(ctx, id)
		if err != nil {
			return false, fmt.Errorf("sep2embed: mirror retention: count readings of %q: %w", id, err)
		}
		prev, known := l.mirrors[id]
		device := prev.device
		if !known {
			if device, err = mirrorDevice(ctx, stores, id); err != nil {
				return false, err
			}
		}
		if int(n) < prev.total {
			return true, nil
		}
		if n > 0 {
			first, err := stores.MirrorMeterReadings.List(ctx, id, store.ListOptions{Limit: 1})
			if err != nil {
				return false, fmt.Errorf("sep2embed: mirror retention: read oldest reading of %q: %w", id, err)
			}
			if len(first.Items) > 0 && first.Items[0].LastUpdateTime < cutoff {
				return true, nil
			}
		}
		next[id] = scannedMirror{device: device, total: int(n)}
		if _, seen := bound[device]; !seen {
			bound[device] = l.peak[device]
		}
		bound[device] += int(n) - prev.total
	}
	for _, b := range bound {
		if b > maxPerSeries {
			return true, nil
		}
	}
	l.mirrors, l.peak = next, bound
	return false, nil
}

// mirrorDevice is the series device of a mirror: its deviceLFDI, or
// "mirror:<id>" when it names none.
func mirrorDevice(ctx context.Context, stores *assembly.Stores, id string) (string, error) {
	switch mup, err := stores.MirrorUsagePoints.Get(ctx, id); {
	case err == nil && mup.DeviceLFDI != "":
		return mup.DeviceLFDI, nil
	case err == nil || isNotFound(err):
		return "mirror:" + id, nil
	default:
		return "", fmt.Errorf("sep2embed: mirror retention: read mirror %q: %w", id, err)
	}
}

// keepTypedReading un-drops the oldest dropped typed record of an mRID when
// every typed record of that mRID is being dropped while an untyped one
// survives. An untyped reading inherits its type from a typed record of the
// same mirror and mRID, so removing the last typed one would leave the
// survivors with no type and drop them out of every series.
func keepTypedReading(recs []mirrorRecord, drop []bool) {
	type mrid struct{ mirror, mrid string }
	type state struct {
		survivor, typedSurvivor bool
		oldestTypedDropped      int
	}
	byMRID := map[mrid]*state{}
	for i, r := range recs {
		k := mrid{r.mirror, r.mmr.MRID}
		s, ok := byMRID[k]
		if !ok {
			s = &state{oldestTypedDropped: -1}
			byMRID[k] = s
		}
		switch {
		case !drop[i]:
			s.survivor = true
			s.typedSurvivor = s.typedSurvivor || r.typed
		case r.typed && s.oldestTypedDropped < 0:
			s.oldestTypedDropped = i
		}
	}
	for _, s := range byMRID {
		if s.survivor && !s.typedSurvivor && s.oldestTypedDropped >= 0 {
			drop[s.oldestTypedDropped] = false
		}
	}
}

// newMirrorTicker is a seam for tests that drive the sweep period.
var newMirrorTicker = time.NewTicker

// mirrorRetentionSettings returns the bounds with their defaults applied.
func (e *Embed) mirrorRetentionSettings() (maxAge time.Duration, maxPerSeries int, interval time.Duration) {
	maxAge = durationOrDefault(e.mirrorMaxAge, DefaultMirrorReadingRetention)
	maxPerSeries = e.mirrorMaxPerSeries
	if maxPerSeries <= 0 {
		maxPerSeries = DefaultMirrorReadingMaxPerSeries
	}
	interval = durationOrDefault(e.mirrorInterval, DefaultMirrorRetentionInterval)
	return maxAge, maxPerSeries, interval
}

// runMirrorRetention sweeps once at start and then every interval until ctx
// is cancelled. A failed sweep is logged and the loop continues: stopping
// would let the store grow again on the strength of one bad listing.
func (e *Embed) runMirrorRetention(ctx context.Context) {
	if e.stores == nil {
		return
	}
	maxAge, maxPerSeries, interval := e.mirrorRetentionSettings()
	led := &sweepLedger{}
	sweep := func() {
		if _, err := sweepMirrorReadings(ctx, e.stores, led, time.Now(), maxAge, maxPerSeries); err != nil && ctx.Err() == nil {
			log.Printf("sep2embed: mirror retention sweep: %v", err)
		}
	}
	sweep()
	ticker := newMirrorTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
