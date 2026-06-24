package main

import (
	"slices"
	"sync"

	"github.com/samber/lo"
)

// TimestampSpeedcam measures the source’s write rate by tallying the
// number of writes seen per unique second. Thus we can measure the
// source’s write load even if the change stream lags.
type TimestampSpeedcam struct {
	mutex sync.Mutex

	currentSec uint32
	count      int
	history    []int
}

func NewTimestampSpeedcam(historySize int) *TimestampSpeedcam {
	return &TimestampSpeedcam{
		history: make([]int, 0, historySize),
	}
}

func (s *TimestampSpeedcam) Add(ts uint32, count int) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	lo.Assertf(
		ts >= s.currentSec,
		"timestamps must increase monotonically (got %d, expected >= %d)",
		ts,
		s.currentSec,
	)

	switch {
	case ts == s.currentSec:
		s.count += count
	case ts > s.currentSec:
		if s.currentSec != 0 {
			for range ts - s.currentSec - 1 {
				s.append(0)
			}
		}

		s.append(s.count)

		s.currentSec = ts
		s.count = count
	default:
		panic("unreachable")
	}
}

func (s *TimestampSpeedcam) GetHistory() []int {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return slices.Clone(s.history)
}

func (s *TimestampSpeedcam) append(count int) {
	if len(s.history) == cap(s.history) {
		s.history = slices.Delete(s.history, 0, 1)
	}

	s.history = append(s.history, count)
}
