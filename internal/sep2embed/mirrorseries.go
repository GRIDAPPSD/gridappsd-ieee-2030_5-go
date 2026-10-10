package sep2embed

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2srv/assembly"
	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/store"
)

// Bounds on one MirrorSeries response. A request may ask for fewer, never
// for more, so a poll cannot make the bridge build an unbounded answer.
const (
	MaxMirrorSeries       = 500
	MaxMirrorPointsSeries = 2000
)

// MirrorPoint is one posted reading: Time is the server's receipt time in
// Unix seconds (never a client field) and Value is the reading scaled by its
// power-of-ten multiplier. ID names the point stably across polls (the stored
// reading id and the point's place in it), so a poller that resumes from an
// inclusive time can drop the points it already has.
type MirrorPoint struct {
	ID    string
	Time  int64
	Value float64
}

// MirrorSeries is every retained point of one device and one reading type.
//
// The type is uom, kind, phase and description, and deliberately not
// flowDirection: a client flips that with the sign of the value, and keying
// on it would split one physical quantity into two lines at every sign
// change. A zero uom, kind or phase is the standard's "not applicable",
// which is also what an absent element means, so the two are not told apart.
//
// DeviceLFDI is the mirror's own deviceLFDI. Mirror is the mirror id and is
// set only when DeviceLFDI is empty, the one case where nothing else names
// the device.
type MirrorSeries struct {
	DeviceLFDI  string
	Mirror      string
	Uom         uint8
	Kind        uint8
	Phase       uint8
	Description string

	// Points are oldest first. When Truncated, only the newest points are
	// here and Total counts every point that matched the query.
	Points    []MirrorPoint
	Total     int
	Truncated bool
}

// MirrorQuery selects and bounds one MirrorSeries answer. Since is inclusive
// Unix seconds. Device, when set, keeps only the series of the device with
// that deviceLFDI (case-insensitive), or of the mirror with that id when the
// mirror names no device; Uom, when set, keeps only that unit code.
// MaxSeries and MaxPoints lower the ceilings and cannot raise them; at or
// below zero they take the ceiling.
type MirrorQuery struct {
	Since     int64
	Device    string
	Uom       *uint8
	MaxSeries int
	MaxPoints int
}

// MirrorSeriesResult is a bounded answer: Series is cut at maxSeries and
// SeriesTruncated says so, with TotalSeries counting every series that had a
// point at or after the query time.
type MirrorSeriesResult struct {
	Series          []MirrorSeries
	TotalSeries     int
	SeriesTruncated bool
}

// seriesKey is the identity of one series. device is the mirror's
// deviceLFDI, or "mirror:<id>" when it has none, so mirrors that name no
// device never merge.
type seriesKey struct {
	device      string
	uom         uint8
	kind        uint8
	phase       uint8
	description string
}

// mirrorRecord is one stored MirrorMeterReading with what a series and the
// retention sweep need from its mirror.
type mirrorRecord struct {
	mirror  string
	id      string
	lfdi    string
	key     seriesKey
	typed   bool
	mmr     sep2.MirrorMeterReading
	effType *sep2.ReadingType
}

// MirrorSeries returns the points posted at or after q.Since, grouped by
// device and reading type. The query time is inclusive so a poll that resumes
// from the last time it saw cannot lose a reading stamped in the same second;
// the caller drops the repeat by MirrorPoint.ID. A reading with no stored
// value is skipped rather than reported as zero. At most q.MaxSeries series
// and q.MaxPoints points per series are returned, newest points kept.
//
// Only the readings from q.Since on are copied out of the store, which relies
// on the id the server stamps: a fixed-width receipt time in nanoseconds.
func (e *Embed) MirrorSeries(ctx context.Context, q MirrorQuery) (MirrorSeriesResult, error) {
	return mirrorSeries(ctx, e.stores, e.mirrorTypes, q)
}

func mirrorSeries(ctx context.Context, stores *assembly.Stores, types *mirrorTypeCache, q MirrorQuery) (MirrorSeriesResult, error) {
	maxSeries, maxPoints := q.MaxSeries, q.MaxPoints
	if maxSeries <= 0 || maxSeries > MaxMirrorSeries {
		maxSeries = MaxMirrorSeries
	}
	if maxPoints <= 0 || maxPoints > MaxMirrorPointsSeries {
		maxPoints = MaxMirrorPointsSeries
	}
	scan, err := collectMirrorRecords(ctx, stores, types, q.Since)
	if err != nil {
		return MirrorSeriesResult{}, err
	}
	recs := scan.records

	byKey := map[seriesKey]*MirrorSeries{}
	for _, r := range recs {
		if q.Device != "" && !strings.EqualFold(r.key.device, q.Device) {
			continue
		}
		if q.Uom != nil && r.key.uom != *q.Uom {
			continue
		}
		pts := readingPoints(r)
		if len(pts) == 0 {
			continue
		}
		s, ok := byKey[r.key]
		if !ok {
			s = &MirrorSeries{
				DeviceLFDI:  r.lfdi,
				Uom:         r.key.uom,
				Kind:        r.key.kind,
				Phase:       r.key.phase,
				Description: r.key.description,
			}
			if r.lfdi == "" {
				s.Mirror = r.mirror
			}
			byKey[r.key] = s
		}
		s.Points = append(s.Points, pts...)
	}

	all := make([]*MirrorSeries, 0, len(byKey))
	for _, s := range byKey {
		all = append(all, s)
	}
	slices.SortFunc(all, func(a, b *MirrorSeries) int {
		return cmp.Or(
			cmp.Compare(a.DeviceLFDI, b.DeviceLFDI),
			cmp.Compare(a.Mirror, b.Mirror),
			cmp.Compare(a.Uom, b.Uom),
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Phase, b.Phase),
			cmp.Compare(a.Description, b.Description),
		)
	})

	res := MirrorSeriesResult{TotalSeries: len(all), Series: []MirrorSeries{}}
	if len(all) > maxSeries {
		all = all[:maxSeries]
		res.SeriesTruncated = true
	}
	for _, s := range all {
		slices.SortStableFunc(s.Points, func(a, b MirrorPoint) int { return cmp.Compare(a.Time, b.Time) })
		s.Total = len(s.Points)
		if len(s.Points) > maxPoints {
			s.Points = slices.Clone(s.Points[len(s.Points)-maxPoints:])
			s.Truncated = true
		}
		res.Series = append(res.Series, *s)
	}
	return res, nil
}

// readingPoints returns the scaled values one stored reading carries, all at
// the reading's receipt time. A reading posted alone and a MirrorReadingSet
// are the two shapes a client uses; both are read.
func readingPoints(r mirrorRecord) []MirrorPoint {
	var mult int8
	if r.effType != nil && r.effType.PowerOfTenMultiplier != nil {
		mult = *r.effType.PowerOfTenMultiplier
	}
	var pts []MirrorPoint
	add := func(rd *sep2.Reading) {
		if rd == nil || rd.Value == nil {
			return
		}
		pts = append(pts, MirrorPoint{ID: fmt.Sprintf("%s/%s.%d", r.mirror, r.id, len(pts)), Time: r.mmr.LastUpdateTime, Value: scaleReading(*rd.Value, mult)})
	}
	add(r.mmr.Reading)
	for i := range r.mmr.MirrorReadingSet {
		for j := range r.mmr.MirrorReadingSet[i].Reading {
			add(&r.mmr.MirrorReadingSet[i].Reading[j])
		}
	}
	return pts
}

// scaleReading applies value * 10^mult. A negative exponent divides by an
// exact power of ten, which rounds once, where multiplying by 0.001 would
// carry the representation error of 0.001 into the result.
func scaleReading(v int64, mult int8) float64 {
	if mult >= 0 {
		return float64(v) * math.Pow10(int(mult))
	}
	return float64(v) / math.Pow10(int(-mult))
}

// mirrorScan is what one pass over the mirror readings found. records are
// ordered by id, which is a fixed-width receipt time in nanoseconds, so id
// order is age order; ties across mirrors break on the mirror id. mirrors
// holds every mirror seen, with its device and, for a full pass only, how
// many readings it stores.
type mirrorScan struct {
	records []mirrorRecord
	mirrors map[string]scannedMirror
}

type scannedMirror struct {
	device string
	total  int
}

// mirrorIDBefore returns the store id just below the first one a reading
// received at the Unix second since can carry, for ListOptions.After. ok is
// false when since is so large that no id can reach it.
func mirrorIDBefore(since int64) (after string, ok bool) {
	const maxSince = math.MaxInt64 / 1_000_000_000
	if since > maxSince {
		return "", false
	}
	return fmt.Sprintf("%020d", since*1_000_000_000-1), true
}

// typeInfo is a reading type and description an mRID carries. typedID is the
// stored reading that carries it, or empty when it came from the mirror's own
// inline readings.
type typeInfo struct {
	rt      *sep2.ReadingType
	desc    string
	typedID string
}

// mirrorTypeCache remembers, per mirror and mRID, the type that a typed
// reading established, so a poll that copies only recent readings can type an
// untyped one without listing the whole mirror again. An entry is trusted
// only while the reading that carries it is still stored.
type mirrorTypeCache struct {
	mu sync.Mutex
	m  map[string]map[string]typeInfo
}

func newMirrorTypeCache() *mirrorTypeCache {
	return &mirrorTypeCache{m: map[string]map[string]typeInfo{}}
}

func (c *mirrorTypeCache) get(mirror, mrid string) (typeInfo, bool) {
	if c == nil {
		return typeInfo{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ti, ok := c.m[mirror][mrid]
	return ti, ok
}

func (c *mirrorTypeCache) put(mirror, mrid string, ti typeInfo) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m[mirror] == nil {
		c.m[mirror] = map[string]typeInfo{}
	}
	c.m[mirror][mrid] = ti
}

func (c *mirrorTypeCache) forget(mirror, mrid string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m[mirror], mrid)
}

// retain drops the entries of every mirror not in keep.
func (c *mirrorTypeCache) retain(keep []string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.m {
		if !slices.Contains(keep, id) {
			delete(c.m, id)
		}
	}
}

// collectMirrorRecords reads the stored mirror readings received at or after
// since (Unix seconds; at or below zero reads them all) with each mirror's
// device and each reading's effective reading type.
//
// A reading that reuses an mRID may omit its ReadingType and inherits it from
// any record of that mRID in the same mirror, inline readings included, so a
// reading's type is its own or the first one its mRID carries. With since
// above zero only recent readings are copied, so that type is looked up in
// types, checked against the store, and failing that learned by reading the
// whole mirror once. A reading with none is typed uom 0, kind 0, phase 0 and
// scaled by 1.
func collectMirrorRecords(ctx context.Context, stores *assembly.Stores, types *mirrorTypeCache, since int64) (mirrorScan, error) {
	scan := mirrorScan{mirrors: map[string]scannedMirror{}}
	if store.IsAbsent(stores.MirrorMeterReadings) || store.IsAbsent(stores.MirrorUsagePoints) {
		return scan, nil
	}
	parents, err := stores.MirrorMeterReadings.Parents(ctx)
	if err != nil {
		return mirrorScan{}, fmt.Errorf("sep2embed: mirror readings: list mirrors: %w", err)
	}
	types.retain(parents)

	windowed := since > 0
	opts := store.ListOptions{Unbounded: true}
	if windowed {
		after, ok := mirrorIDBefore(since)
		if !ok {
			return scan, nil
		}
		opts.After = after
	}

	for _, mupID := range parents {
		if err := ctx.Err(); err != nil {
			return mirrorScan{}, err
		}
		var lfdi string
		var inline []sep2.MirrorMeterReading
		switch mup, err := stores.MirrorUsagePoints.Get(ctx, mupID); {
		case err == nil:
			lfdi, inline = mup.DeviceLFDI, mup.MirrorMeterReading
		case isNotFound(err):
		default:
			return mirrorScan{}, fmt.Errorf("sep2embed: mirror readings: read mirror %q: %w", mupID, err)
		}
		page, err := stores.MirrorMeterReadings.List(ctx, mupID, opts)
		if err != nil {
			return mirrorScan{}, fmt.Errorf("sep2embed: mirror readings: list readings of %q: %w", mupID, err)
		}

		device := lfdi
		if device == "" {
			device = "mirror:" + mupID
		}
		scan.mirrors[mupID] = scannedMirror{device: device, total: len(page.Items)}

		known := map[string]typeInfo{}
		learn := func(m sep2.MirrorMeterReading, typedID string) {
			if m.ReadingType == nil {
				return
			}
			if _, ok := known[m.MRID]; !ok {
				known[m.MRID] = typeInfo{rt: m.ReadingType, desc: m.Description, typedID: typedID}
			}
		}
		learnPage := func(items []sep2.MirrorMeterReading) {
			for _, m := range items {
				id, _ := mirrorReadingID(mupID, m.Href)
				learn(m, id)
			}
		}
		for _, m := range inline {
			learn(m, "")
		}
		learnPage(page.Items)

		if windowed {
			missing := false
			for _, m := range page.Items {
				if m.ReadingType != nil {
					continue
				}
				if _, ok := known[m.MRID]; ok {
					continue
				}
				ti, ok, err := cachedType(ctx, stores, types, mupID, m.MRID)
				if err != nil {
					return mirrorScan{}, err
				}
				if ok {
					known[m.MRID] = ti
					continue
				}
				missing = true
			}
			if missing {
				all, err := stores.MirrorMeterReadings.List(ctx, mupID, store.ListOptions{Unbounded: true})
				if err != nil {
					return mirrorScan{}, fmt.Errorf("sep2embed: mirror readings: list readings of %q: %w", mupID, err)
				}
				learnPage(all.Items)
			}
		}
		for mrid, ti := range known {
			if ti.typedID != "" {
				types.put(mupID, mrid, ti)
			}
		}

		for _, m := range page.Items {
			if m.LastUpdateTime < since {
				continue
			}
			id, ok := mirrorReadingID(mupID, m.Href)
			if !ok {
				continue
			}
			rt, desc := m.ReadingType, m.Description
			if rt == nil {
				ti := known[m.MRID]
				rt = ti.rt
				if desc == "" {
					desc = ti.desc
				}
			}
			k := seriesKey{device: device, description: desc}
			if rt != nil {
				if rt.Uom != nil {
					k.uom = *rt.Uom
				}
				if rt.Kind != nil {
					k.kind = *rt.Kind
				}
				if rt.Phase != nil {
					k.phase = *rt.Phase
				}
			}
			scan.records = append(scan.records, mirrorRecord{mirror: mupID, id: id, lfdi: lfdi, key: k, typed: m.ReadingType != nil, mmr: m, effType: rt})
		}
	}
	slices.SortStableFunc(scan.records, func(a, b mirrorRecord) int {
		return cmp.Or(cmp.Compare(a.id, b.id), cmp.Compare(a.mirror, b.mirror))
	})
	return scan, nil
}

// cachedType returns the cached type of an mRID after checking that the
// reading that established it is still stored. A reading that is gone, or no
// longer typed, drops the entry.
func cachedType(ctx context.Context, stores *assembly.Stores, types *mirrorTypeCache, mirror, mrid string) (typeInfo, bool, error) {
	ti, ok := types.get(mirror, mrid)
	if !ok {
		return typeInfo{}, false, nil
	}
	switch m, err := stores.MirrorMeterReadings.Get(ctx, mirror, ti.typedID); {
	case err == nil && m.ReadingType != nil && m.MRID == mrid:
		return ti, true, nil
	case err == nil || isNotFound(err):
		types.forget(mirror, mrid)
		return typeInfo{}, false, nil
	default:
		return typeInfo{}, false, fmt.Errorf("sep2embed: mirror readings: read reading %s/%s: %w", mirror, ti.typedID, err)
	}
}

// mirrorReadingID returns the store id of a reading stamped
// /mup/{mupID}/mr/{id}. A record without that server-stamped href cannot be
// addressed, so it is left out rather than guessed at.
func mirrorReadingID(mupID, href string) (string, bool) {
	id, ok := strings.CutPrefix(href, "/mup/"+mupID+"/mr/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}
