package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/FGasper/mongo-speedcam/agg"
	"github.com/FGasper/mongo-speedcam/cursor"
	"github.com/FGasper/mongo-speedcam/history"
	"github.com/FGasper/mongo-speedcam/resumetoken"
	"github.com/mongodb-labs/migration-tools/humantools"
	"github.com/mongodb-labs/migration-tools/mongotools/changestream"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func _runChangeStream(ctx context.Context, connstr string, interval time.Duration) error {
	client, err := getClient(connstr)
	if err != nil {
		return err
	}

	sess, err := client.StartSession()
	if err != nil {
		return fmt.Errorf("opening session: %w", err)
	}

	sctx := mongo.NewSessionContext(ctx, sess)

	unixTimeStart := uint32(time.Now().Add(-interval).Unix())
	startTS := bson.Timestamp{T: unixTimeStart}

	db := client.Database("admin")

	fmt.Printf("Gathering change events from the past %s …\n", interval)

	startTime := time.Now()

	resp := db.RunCommand(
		sctx,
		bson.D{
			{"aggregate", 1},
			{"cursor", bson.D{}},
			{"pipeline", mongo.Pipeline{
				{{"$changeStream", bson.D{
					{"allChangesForCluster", true},
					//{"showSystemEvents", true},
					//{"showExpandedEvents", true},
					{"startAtOperationTime", startTS},
				}}},
				{{"$match", bson.D{
					{"clusterTime", bson.D{
						{"$lte", bson.Timestamp{T: uint32(time.Now().Unix())}},
					}},
				}}},
				{{"$project", bson.D{
					{"_id", 1},
					{"clusterTime", 1},

					{"op", bson.D{{"$cond", bson.D{
						{"if", bson.D{{"$in", [2]any{
							"$operationType",
							eventsToTruncate,
						}}}},
						{"then", bson.D{{"$substr",
							[3]any{"$operationType", 0, 1},
						}}},
						{"else", "$operationType"},
					}}}},

					{"size", bson.D{{"$bsonSize", "$$ROOT"}}},
				}}},
			}},
		},
	)

	cursor, err := cursor.New(db, resp)
	if err != nil {
		return fmt.Errorf("opening change stream: %w", err)
	}

	eventSizesByType := map[string]int{}
	eventCountsByType := map[string]int{}

	fullEventName := map[string]string{}
	for _, eventName := range eventsToTruncate {
		fullEventName[eventName[:1]] = eventName
	}

cursorLoop:
	for {
		if cursor.IsFinished() {
			return fmt.Errorf("unexpected end of change stream")
		}

		for _, event := range cursor.GetCurrentBatch() {
			t, _ := event.Lookup("clusterTime").Timestamp()

			if time.Unix(int64(t), 0).After(startTime) {
				break cursorLoop
			}

			op := event.Lookup("op").StringValue()

			if fullOp, isShortened := fullEventName[op]; isShortened {
				op = fullOp
			}

			eventCountsByType[op]++
			eventSizesByType[op] += int(event.Lookup("size").AsInt64())
		}

		rt, hasToken := cursor.GetCursorExtra()["postBatchResumeToken"]
		if !hasToken {
			return fmt.Errorf("change stream lacks resume token??")
		}

		tokenTS, err := resumetoken.New(rt.Document()).Timestamp()
		if err != nil {
			return fmt.Errorf("parsing timestamp from change stream resume token")
		}

		if time.Unix(int64(tokenTS.T), 0).After(startTime) {
			break cursorLoop
		}

		if err := cursor.GetNext(sctx); err != nil {
			return fmt.Errorf("iterating change stream: %w", err)
		}
	}

	displayTable(eventCountsByType, eventSizesByType, interval)

	return nil
}

func _runTailChangeStream(
	ctx context.Context,
	connstr string,
	window, reportInterval time.Duration,
	updateLookup bool,
	streams uint8,
) error {
	client, err := getClient(connstr)
	if err != nil {
		return err
	}

	sess, err := client.StartSession()
	if err != nil {
		return fmt.Errorf("opening session: %w", err)
	}

	sctx := mongo.NewSessionContext(ctx, sess)

	tsSpeedcam := NewTimestampSpeedcam(int(window.Seconds()))

	cs, err := changestream.NewParallel(
		sctx,
		client,
		changestream.Options{
			Streams: int(streams),
			Pipeline: mongo.Pipeline{
				{{"$project", bson.D{
					{"_id", 1},
					{"clusterTime", 1},
					{"fullDocument", 1},
					{"op", agg.Cond{
						If:   agg.In("$operationType", eventsToTruncate...),
						Then: agg.SubstrBytes{"$operationType", 0, 1},
						Else: "$operationType",
					}},
					{"size", agg.BSONSize("$$ROOT")},
				}}},
			},
			Options: options.ChangeStream().SetFullDocument(
				lo.Ternary(updateLookup, options.UpdateLookup, options.Default),
			),
		},
	)
	if err != nil {
		return fmt.Errorf("open change stream: %w", err)
	}

	defer cs.Close()

	fmt.Printf("Listening for change events. Stats showing every %s …\n", reportInterval)

	eventsHistory := history.New[eventStats](window)

	var changeStreamLag atomic.Pointer[time.Duration]

	startTime := time.Now()

	go func() {
		for {
			time.Sleep(reportInterval)

			totalStats, _, _ := tallyEventsHistory(eventsHistory)

			averagePeriod := min(window, time.Since(startTime))

			displayTable(totalStats.counts, totalStats.sizes, averagePeriod)

			perSecEventCounts := tsSpeedcam.GetHistory()
			if len(perSecEventCounts) > 0 {
				// drop current second, which may be incomplete
				perSecEventCounts = perSecEventCounts[:len(perSecEventCounts)-1]
			}
			eventsPerSec := lo.Mean(perSecEventCounts)

			fmt.Printf(
				"Change stream lag: %s (%s ops/sec seen on source)\n",
				lo.FromPtr(changeStreamLag.Load()),
				humantools.FmtReal(eventsPerSec),
			)
		}
	}()

	fullEventName := map[string]string{}
	for _, eventName := range eventsToTruncate {
		fullEventName[eventName[:1]] = eventName
	}

	var curEventStats eventStats
	initMap(&curEventStats.counts)
	initMap(&curEventStats.sizes)

	for {
		if !cs.TryNext(sctx) {
			if cs.Err() != nil {
				return fmt.Errorf("reading change stream: %w", cs.Err())
			}

			// If we got an empty batch, then assume no lag.
			changeStreamLag.Store(lo.ToPtr(time.Duration(0)))

			eventsHistory.Add(curEventStats)
			initMap(&curEventStats.counts)
			initMap(&curEventStats.sizes)

			sessTS, err := GetClusterTimeFromSession(sess)
			if err != nil {
				fmt.Printf("------ getting cluster time from session: %s\n", err)
			} else {
				tsSpeedcam.Add(sessTS.T, 0)
			}

			continue
		}

		op := cs.Current().Lookup("op").StringValue()

		if fullOp, isShortened := fullEventName[op]; isShortened {
			op = fullOp
		}

		curEventStats.counts[op]++
		curEventStats.sizes[op] += int(cs.Current().Lookup("size").AsInt64())

		// Every 100 events, snapshot the current stats and reset the counters.
		// This lets us keep a history of event counts and sizes over time
		// without unbounded memory growth.
		if rand.Float64() < 0.01 {
			eventsHistory.Add(curEventStats)
			initMap(&curEventStats.counts)
			initMap(&curEventStats.sizes)
		}

		sessTS, err := GetClusterTimeFromSession(sess)
		if err != nil {
			fmt.Printf("------ getting cluster time from session: %s\n", err)
		} else {
			eventT, _ := cs.Current().Lookup("clusterTime").Timestamp()

			tsSpeedcam.Add(eventT, 1)

			lagSecs := int64(sessTS.T) - int64(eventT)
			changeStreamLag.Store(lo.ToPtr(time.Duration(lagSecs) * time.Second))
		}
	}
}
