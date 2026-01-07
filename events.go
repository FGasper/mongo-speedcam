package main

import (
	"time"

	"github.com/FGasper/mongo-speedcam/history"
)

type eventStats struct {
	sizes, counts map[string]int
}

func tallyEventsHistory(
	eventsHistory *history.History[eventStats],
) (eventStats, int, time.Duration) {
	eventsInWindow := eventsHistory.Get()

	totalStats := eventStats{}
	initMap(&totalStats.counts)
	initMap(&totalStats.sizes)

	for _, curLog := range eventsInWindow {
		for evtType, val := range curLog.Datum.counts {
			if _, ok := totalStats.counts[evtType]; !ok {
				totalStats.counts[evtType] = val
			} else {
				totalStats.counts[evtType] += val
			}
		}

		for evtType, val := range curLog.Datum.sizes {
			if _, ok := totalStats.sizes[evtType]; !ok {
				totalStats.sizes[evtType] = val
			} else {
				totalStats.sizes[evtType] += val
			}

		}
	}

	var curStatsInterval time.Duration

	if len(eventsInWindow) > 0 {
		curStatsInterval = time.Since(eventsInWindow[0].At)
	}

	return totalStats, len(eventsInWindow), curStatsInterval
}
