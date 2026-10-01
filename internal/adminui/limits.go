package adminui

import (
	"encoding/json"
	"fmt"

	"github.com/GRIDAPPSD/ieee-2030_5-server-go/pkg/sep2admin"
)

// The plane answers 500 for a Descriptor over either cap, counted across
// all of its sections, so a panel trims itself to fit instead. The values
// copy the plane's unexported maxPanelRows and maxPanelBytes;
// TestPanelsNeverFailOnRowCount fails if the plane's row cap shrinks.
const (
	panelMaxRows  = 1000
	panelMaxBytes = 1 << 20
)

// tableSpec is a table section before it is fitted to the caps.
type tableSpec struct {
	heading, empty string
	prose          []string
	columns        []string
	rows           []sep2admin.Row
}

func table(heading, empty string, prose []string, columns []string, rows []sep2admin.Row) tableSpec {
	return tableSpec{heading: heading, empty: empty, prose: prose, columns: columns, rows: rows}
}

// tables builds a Descriptor of table sections that the plane will serve:
// the row budget is shared fairly between the sections, and halved until
// the encoding fits the byte cap. A section that lost rows says so.
func tables(specs ...tableSpec) sep2admin.Descriptor {
	lens := make([]int, len(specs))
	for i, s := range specs {
		lens[i] = len(s.rows)
	}
	budget := panelMaxRows
	for {
		limits := shareRows(lens, budget)
		d := buildTables(specs, limits)
		// An encode error is left for the plane to report, as it would for
		// any panel.
		b, err := json.Marshal(d)
		if err != nil || len(b) <= panelMaxBytes || budget == 0 {
			return d
		}
		budget = sum(limits) / 2
	}
}

func buildTables(specs []tableSpec, limits []int) sep2admin.Descriptor {
	sections := make([]sep2admin.Section, 0, len(specs))
	for i, s := range specs {
		prose, empty := s.prose, s.empty
		if limits[i] < len(s.rows) {
			note := fmt.Sprintf("Showing %d of %d rows: a panel holds at most %d rows and %d KiB.",
				limits[i], len(s.rows), panelMaxRows, panelMaxBytes>>10)
			prose = append(append([]string(nil), prose...), note)
			empty = note
		}
		sections = append(sections, sep2admin.Section{
			Heading: s.heading,
			Prose:   prose,
			Empty:   empty,
			Body:    sep2admin.NewTableBody(sep2admin.TableBody{Columns: s.columns, Rows: s.rows[:limits[i]]}),
		})
	}
	return descriptor(sections...)
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
