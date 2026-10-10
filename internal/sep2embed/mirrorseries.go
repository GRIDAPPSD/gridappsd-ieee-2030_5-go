package sep2embed

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

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
// power-of-ten multiplier.
type MirrorPoint struct {
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

// MirrorSeries returns the points posted at or after since (Unix seconds),
// grouped by device and reading type. The query time is inclusive so a poll
// that resumes from the last time it saw cannot lose a reading stamped in the
// same second; the caller drops the repeat. A reading with no stored value is
// skipped rather than reported as zero. At most maxSeries series and
// maxPoints points per series are returned, newest points kept; a request
// above MaxMirrorSeries or MaxMirrorPointsSeries, or at or below zero, takes
// the maximum.
func (e *Embed) MirrorSeries(ctx context.Context, since int64, maxSeries, maxPoints int) (MirrorSeriesResult, error) {
	return mirrorSeries(ctx, e.stores, since, maxSeries, maxPoints)
}

func mirrorSeries(ctx context.Context, stores *assembly.Stores, since int64, maxSeries, maxPoints int) (MirrorSeriesResult, error) {
	if maxSeries <= 0 || maxSeries > MaxMirrorSeries {
		maxSeries = MaxMirrorSeries
	}
	if maxPoints <= 0 || maxPoints > MaxMirrorPointsSeries {
		maxPoints = MaxMirrorPointsSeries
	}
	recs, err := collectMirrorRecords(ctx, stores)
	if err != nil {
		return MirrorSeriesResult{}, err
	}

	byKey := map[seriesKey]*MirrorSeries{}
	for _, r := range recs {
		if r.mmr.LastUpdateTime < since {
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
		pts = append(pts, MirrorPoint{Time: r.mmr.LastUpdateTime, Value: scaleReading(*rd.Value, mult)})
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

// collectMirrorRecords reads every stored mirror reading with its mirror's
// device and its effective reading type. Records are ordered by id, which is
// a fixed-width receipt time in nanoseconds, so id order is age order; ties
// across mirrors break on the mirror id.
//
// A reading that reuses an mRID may omit its ReadingType and inherits it from
// any record of that mRID in the same mirror, inline readings included, so a
// reading's type is its own or the first one its mRID carries. A reading with
// none is typed uom 0, kind 0, phase 0 and scaled by 1.
func collectMirrorRecords(ctx context.Context, stores *assembly.Stores) ([]mirrorRecord, error) {
	if store.IsAbsent(stores.MirrorMeterReadings) || store.IsAbsent(stores.MirrorUsagePoints) {
		return nil, nil
	}
	parents, err := stores.MirrorMeterReadings.Parents(ctx)
	if err != nil {
		return nil, fmt.Errorf("sep2embed: mirror readings: list mirrors: %w", err)
	}
	var out []mirrorRecord
	for _, mupID := range parents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var lfdi string
		var inline []sep2.MirrorMeterReading
		switch mup, err := stores.MirrorUsagePoints.Get(ctx, mupID); {
		case err == nil:
			lfdi, inline = mup.DeviceLFDI, mup.MirrorMeterReading
		case isNotFound(err):
		default:
			return nil, fmt.Errorf("sep2embed: mirror readings: read mirror %q: %w", mupID, err)
		}
		page, err := stores.MirrorMeterReadings.List(ctx, mupID, store.ListOptions{Unbounded: true})
		if err != nil {
			return nil, fmt.Errorf("sep2embed: mirror readings: list readings of %q: %w", mupID, err)
		}

		types := map[string]*sep2.ReadingType{}
		descs := map[string]string{}
		learn := func(m sep2.MirrorMeterReading) {
			if m.ReadingType == nil {
				return
			}
			if _, ok := types[m.MRID]; !ok {
				types[m.MRID] = m.ReadingType
				descs[m.MRID] = m.Description
			}
		}
		for _, m := range inline {
			learn(m)
		}
		for _, m := range page.Items {
			learn(m)
		}

		device := lfdi
		if device == "" {
			device = "mirror:" + mupID
		}
		for _, m := range page.Items {
			id, ok := mirrorReadingID(mupID, m.Href)
			if !ok {
				continue
			}
			rt, desc := m.ReadingType, m.Description
			if rt == nil {
				rt = types[m.MRID]
				if desc == "" {
					desc = descs[m.MRID]
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
			out = append(out, mirrorRecord{mirror: mupID, id: id, lfdi: lfdi, key: k, typed: m.ReadingType != nil, mmr: m, effType: rt})
		}
	}
	slices.SortStableFunc(out, func(a, b mirrorRecord) int {
		return cmp.Or(cmp.Compare(a.id, b.id), cmp.Compare(a.mirror, b.mirror))
	})
	return out, nil
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
