package raft

import (
	crand "crypto/rand"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"time"
)

func min(a, b uint64) uint64 {
	if a <= b {
		return a
	}
	return b
}

func max(a, b uint64) uint64 {
	if a >= b {
		return a
	}
	return b
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// safeTimer ------------------------------------------------------

type safeTimer struct {
	timer *time.Timer
	C     <-chan time.Time

	// active is true if timer is started, but not yet received from channel.
	// NOTE: must be set to false, after receiving from channel
	active bool
}

// newSafeTimer creates stopped timer
func newSafeTimer() *safeTimer {
	t := time.NewTimer(0)
	if !t.Stop() {
		<-t.C
	}
	return &safeTimer{t, t.C, false}
}

func (t *safeTimer) stop() {
	if !t.timer.Stop() {
		if t.active {
			<-t.C
		}
	}
	t.active = false
}

func (t *safeTimer) reset(d time.Duration) {
	t.stop()
	t.timer.Reset(d)
	t.active = true
}

// backOff ------------------------------------------------

const (
	maxFailureScale = 12
	failureWait     = 10 * time.Millisecond
)

// backOff is used to compute an exponential backOff
// duration. Base time is scaled by the current round,
// up to some maximum scale factor.
func backOff(round uint64) time.Duration {
	base, limit := failureWait, uint64(maxFailureScale)
	power := min(round, limit)
	for power > 2 {
		base *= 2
		power--
	}
	return base
}

// randTime -----------------------------------------------------------------

type randTime struct {
	r *rand.Rand
}

func newRandTime() randTime {
	var seed int64
	if r, err := crand.Int(crand.Reader, big.NewInt(math.MaxInt64)); err != nil {
		seed = time.Now().UnixNano()
	} else {
		seed = r.Int64()
	}
	return randTime{rand.New(rand.NewSource(seed))}
}

func (rt randTime) duration(min time.Duration) time.Duration {
	return min + time.Duration(rt.r.Int63())%min
}

func (rt randTime) after(min time.Duration) <-chan time.Time {
	return time.After(rt.duration(min))
}

// -------------------------------------------------------------------------

type decrUint64Slice []uint64

func (s decrUint64Slice) Len() int           { return len(s) }
func (s decrUint64Slice) Less(i, j int) bool { return s[i] > s[j] }
func (s decrUint64Slice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// -------------------------------------------------------------------------

func bug(format string, v ...interface{}) error {
	return fmt.Errorf("[BUG] "+format, v...)
}

func toErr(v interface{}) error {
	if v != nil {
		if _, ok := v.(error); ok {
			return v.(error)
		} else {
			return fmt.Errorf("unexpected error: %v", v)
		}
	}
	return nil
}
