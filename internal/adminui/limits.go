package adminui

import (
	"encoding/json"
	"fmt"
)

// The plane answers 500 for a Descriptor over either cap, counted across
// all of its sections, so a panel trims itself to fit instead. The values
// copy the plane's unexported maxPanelRows and maxPanelBytes;
// TestPanelsNeverFailOnRowCount fails if the plane's row cap shrinks. The
// page panels keep the same caps, so a page answers what a panel did.
const (
	panelMaxRows  = 1000
	panelMaxBytes = 1 << 20
)

// fitToCaps builds with the row budget shared across tables of the given
// lengths, and halves the budget until the encoding fits the byte cap.
func fitToCaps[T any](lens []int, build func(limits []int) T) T {
	budget := panelMaxRows
	for {
		limits := shareRows(lens, budget)
		d := build(limits)
		// An encode error is left for the caller to report, as the plane
		// would for any panel.
		b, err := json.Marshal(d)
		if err != nil || len(b) <= panelMaxBytes || budget == 0 {
			return d
		}
		budget = sum(limits) / 2
	}
}

// trimNote is the sentence a section carries when it lost rows to the caps.
func trimNote(shown, total int) string {
	return fmt.Sprintf("Showing %d of %d rows: a panel holds at most %d rows and %d KiB.",
		shown, total, panelMaxRows, panelMaxBytes>>10)
}

// shareRows splits budget across tables of the given lengths: a table
// that needs less than an equal share gets all of its rows, and what it
// leaves over is shared among the rest, so one large table cannot starve
// the others.
func shareRows(lens []int, budget int) []int {
	limits := make([]int, len(lens))
	pending := make([]int, 0, len(lens))
	for i, n := range lens {
		if n > 0 {
			pending = append(pending, i)
		}
	}
	for len(pending) > 0 {
		share := budget / len(pending)
		next := pending[:0:0]
		for _, i := range pending {
			if lens[i] <= share {
				limits[i] = lens[i]
				budget -= lens[i]
			} else {
				next = append(next, i)
			}
		}
		if len(next) == len(pending) {
			for k, i := range pending {
				limits[i] = share
				if k < budget%len(pending) {
					limits[i]++
				}
			}
			break
		}
		pending = next
	}
	return limits
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}
