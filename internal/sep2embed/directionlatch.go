package sep2embed

import (
	"sync"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// directionLatch records, per device, the direction of the last non-zero
// opModTargetW this Embed issued and whether the last direction change was
// charge to discharge. The zero value is ready.
type directionLatch struct {
	mu     sync.Mutex
	byMRID map[string]latchState
}

type latchState struct {
	// last is -1 after a charge target, 1 after a discharge target, 0 before
	// any.
	last int8
	pink bool
}

// note applies one issued control. A 0 W target (a stop or a follow-on)
// changes nothing, so pink survives it; only a sign change moves the latch.
func (l *directionLatch) note(mrid string, c sep2.DERControl) {
	sign := targetSign(c)
	if sign == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byMRID == nil {
		l.byMRID = map[string]latchState{}
	}
	st := l.byMRID[mrid]
	if sign != st.last {
		st.pink = st.last < 0 && sign > 0
		st.last = sign
	}
	l.byMRID[mrid] = st
}

func (l *directionLatch) pink(mrid string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byMRID[mrid].pink
}

// targetSign is the sign of the control's opModTargetW, 0 when it carries
// none or a zero value.
func targetSign(c sep2.DERControl) int8 {
	if c.DERControlBase == nil || c.DERControlBase.OpModTargetW == nil {
		return 0
	}
	switch v := c.DERControlBase.OpModTargetW.Value; {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

// DirectionPink reports whether the last direction change this Embed issued
// for the device was charge to discharge. It is kept through a 0 W control and
// cleared by the next direction change. It is in memory, so a restart clears
// it.
func (e *Embed) DirectionPink(deviceMRID string) bool {
	return e.latch.pink(deviceMRID)
}
