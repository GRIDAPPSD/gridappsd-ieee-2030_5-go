package sep2embed

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// BenchmarkMirrorFleet measures one poll and one idle sweep over the store a
// 49-device fleet fills in six hours: 49 x 3 series x 720 readings, 105,840
// in all, only the first post of each series carrying its reading type.
func BenchmarkMirrorFleet(b *testing.B) {
	st := newStores()
	now := int64(1_800_000_000)
	types := []struct {
		mrid, desc string
		uom, kind  uint8
	}{{"w", "Real Power (W)", 38, 37}, {"v", "PH_ABC (V)", 29, 0}, {"q", "Reactive Power (var)", 63, 37}}
	var n int64
	for d := 0; d < 49; d++ {
		mup := fmt.Sprintf("m%02d", d)
		seedMirror(b, st, mup, "LFDI-"+mup)
		for i := 0; i < 720; i++ {
			ts := now - int64(719-i)*30
			for _, t := range types {
				n++
				rt := rtype(t.uom, t.kind, 0, 0, 1)
				if i != 0 {
					rt = nil
				}
				postReading(b, st, mup, n, ts, powerReading(t.mrid, t.desc, rt, n))
			}
		}
	}
	ctx := context.Background()
	cache := newMirrorTypeCache()
	if res, err := mirrorSeries(ctx, st, cache, MirrorQuery{Since: now - 5}); err != nil || res.TotalSeries != 147 {
		b.Fatalf("poll sees %d series (err %v), want the fleet's 147", res.TotalSeries, err)
	}
	b.Run("poll-since-now-minus-5s", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := mirrorSeries(ctx, st, cache, MirrorQuery{Since: now - 5, MaxSeries: 100, MaxPoints: 500}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("sweep-nothing-due", func(b *testing.B) {
		led := &sweepLedger{}
		sweepAt := time.Unix(now, 0)
		if _, err := sweepMirrorReadings(ctx, st, led, sweepAt, 6*time.Hour+time.Minute, 1440); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := sweepMirrorReadings(ctx, st, led, sweepAt, 6*time.Hour+time.Minute, 1440); err != nil {
				b.Fatal(err)
			}
		}
	})
}
