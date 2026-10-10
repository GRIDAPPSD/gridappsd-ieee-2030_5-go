package sep2embed

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/assembly"
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
// A reading that cannot be deleted is logged and left for the next sweep; a
// failure to list the readings is returned and nothing is removed, since an
// incomplete listing would cap the wrong records.
func sweepMirrorReadings(ctx context.Context, stores *assembly.Stores, now time.Time, maxAge time.Duration, maxPerSeries int) (int, error) {
	if maxAge <= 0 || maxPerSeries <= 0 {
		return 0, errors.New("sep2embed: mirror retention needs a positive age and per-series count")
	}
	recs, err := collectMirrorRecords(ctx, stores)
	if err != nil {
		return 0, err
	}

	cutoff := now.Add(-maxAge).Unix()
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
		case isNotFound(err):
		default:
			failed++
			log.Printf("sep2embed: mirror retention: leaving reading %s/%s for the next sweep: %v", r.mirror, r.id, err)
		}
	}
	if removed > 0 || failed > 0 {
		log.Printf("sep2embed: mirror retention: removed=%d left=%d", removed, failed)
	}
	return removed, nil
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
	sweep := func() {
		if _, err := sweepMirrorReadings(ctx, e.stores, time.Now(), maxAge, maxPerSeries); err != nil && ctx.Err() == nil {
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
