package main

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/FGasper/mongo-speedcam/agg"
	"github.com/FGasper/mongo-speedcam/cursor"
	"github.com/FGasper/mongo-speedcam/history"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

var eventsToTruncate = sliceOf(
	"insert",
	"update",
	"replace",
	"delete",
)

var oplogOps = sliceOf("i", "u", "d")

var oplogOpsWithApplyOpsPrefix = lo.Flatten(
	sliceOf(
		oplogOps,
		lo.Map(
			oplogOps,
			func(op string, _ int) string { return "applyOps." + op },
		),
	),
)

func makeOplogSingleOpFilterExpr(opRef string, opsToAccept []string) any {
	return agg.And{
		agg.In(opRef+".op", opsToAccept...),
		agg.Not{agg.Eq(
			agg.SubstrBytes{opRef + ".ns", 0, 7},
			"config.",
		)},
		agg.Not{agg.Eq(
			agg.SubstrBytes{opRef + ".ns", 0, 6},
			"admin.",
		)},
	}
}

var oplogQueryExpr = agg.Or{
	makeOplogSingleOpFilterExpr("$$ROOT", oplogOps),
	agg.And{
		agg.Eq("$op", "c"),
		agg.Eq("$ns", "admin.$cmd"),
		agg.Eq(agg.Type("$o.applyOps"), "array"),
	},
}

// This appends /u or /r to “u” oplog events.
func makeSuffixedOpFieldExpr(opRef string) bson.D {
	return agg.Cond{
		If: agg.In(opRef+".op", "u", "applyOps.u"),
		Then: agg.Concat(
			opRef+".op",
			"/",
			agg.Cond{
				If:   agg.Eq("missing", agg.Type(opRef+".o._id")),
				Then: "u",
				Else: "r",
			},
		),
		Else: opRef + ".op",
	}.D()
}

var oplogUnrollFormatStages = mongo.Pipeline{
	// Match op in ["i", "u", "d"] or applyOps array
	{{"$match", bson.D{{"$expr", oplogQueryExpr}}}},

	// Normalize to ops array
	{{"$addFields", bson.D{
		{"ops", agg.Cond{
			If: agg.Eq("$op", "c"),
			Then: agg.Map{
				Input: agg.Filter{
					Input: "$o.applyOps",
					As:    "opEntry",
					Cond:  makeOplogSingleOpFilterExpr("$$opEntry", oplogOps),
				},
				As: "opEntry",
				In: agg.MergeObjects{
					"$$opEntry",
					bson.D{
						{"op", agg.Concat(
							"applyOps.",
							"$$opEntry.op",
						)},
					},
				},
			},
			Else: bson.A{"$$ROOT"},
		}},
	}}},

	// Unwind ops
	{{"$unwind", "$ops"}},

	// Match sub-ops of interest
	{{"$match", bson.D{
		{"$expr", makeOplogSingleOpFilterExpr("$ops", oplogOpsWithApplyOpsPrefix)},
	}}},

	// Add BSON size per op, and append either /u or /r to op:u.
	{{"$addFields", bson.D{
		{"size", bson.D{{"$bsonSize", "$ops"}}},
		{"ops.op", makeSuffixedOpFieldExpr("$ops")},
	}}},
}

func _runTailOplogMode(
	ctx context.Context,
	connstr string,
	window time.Duration,
	reportInterval time.Duration,
) error {
	if reportInterval > window {
		return fmt.Errorf("report interval must not exceed window")
	}

	client, err := getClient(connstr)
	if err != nil {
		return err
	}

	isSharded, err := isSharded(ctx, client)
	if err != nil {
		return fmt.Errorf("determining whether cluster is sharded: %w", err)
	}

	if isSharded {
		return fmt.Errorf("cannot tail oplog on a sharded cluster")
	}

	startUnixTime := time.Now().Unix()

	db := client.Database("local")

	resp := db.RunCommand(
		ctx,
		bson.D{
			{"find", "oplog.rs"},
			{"filter", agg.And{
				bson.D{
					{"ts", bson.D{
						{"$gte", bson.Timestamp{T: uint32(startUnixTime)}},
					}},
				},
				bson.D{{"$expr", agg.Or{
					oplogQueryExpr,
					agg.Eq("$op", "n"),
				}}},
			}},
			{"tailable", true},
			{"awaitData", true},
			{"projection", bson.D{
				// for debugging:
				//{"o", 1},

				{"ns", 1},

				{"ts", 1},

				{"op", makeSuffixedOpFieldExpr("$$ROOT")},

				{"size", agg.Cond{
					If:   agg.Eq("$op", "c"),
					Then: "$$REMOVE",
					Else: agg.BSONSize("$$ROOT"),
				}},

				{"ops", agg.Cond{
					If: agg.Eq("$op", "c"),
					Then: agg.Map{
						Input: "$o.applyOps",
						As:    "opEntry",
						In: bson.D{
							{"ns", "$$opEntry.ns"},
							{"op", makeSuffixedOpFieldExpr("$$opEntry")},
							{"size", agg.BSONSize("$$opEntry")},
						},
					},
					Else: "$$REMOVE",
				}},
			}},
		},
	)

	cursor, err := cursor.New(db, resp)

	if err != nil {
		return fmt.Errorf("opening oplog cursor: %w", err)
	}

	fmt.Printf("Listening for oplog events. Stats showing every %s …\n\n", reportInterval)

	prodEventsHistory := history.New[eventStats](window)
	mdbEventsHistory := history.New[eventStats](window)

	var lag atomic.Pointer[time.Duration]

	go func() {
		ticker := time.NewTicker(reportInterval)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				showedMDBInternal := false

				totalStats, _, _ := tallyEventsHistory(mdbEventsHistory)
				if len(totalStats.counts) > 0 {
					fmt.Printf("DB-internal:\n")
					displayTable(totalStats.counts, totalStats.sizes, window)
					fmt.Printf("\n")

					showedMDBInternal = true
				}

				if showedMDBInternal {
					fmt.Printf("Production:\n")
				}

				totalStats, _, _ = tallyEventsHistory(prodEventsHistory)
				displayTable(totalStats.counts, totalStats.sizes, window)

				fmt.Printf("Lag: %s\n\n", lo.FromPtr(lag.Load()))
			}
		}
	}()

	for !cursor.IsFinished() {
		batch := cursor.GetCurrentBatch()

		curProdEventStats := eventStats{}
		initMap(&curProdEventStats.counts)
		initMap(&curProdEventStats.sizes)

		curMDBEventStats := eventStats{}
		initMap(&curMDBEventStats.counts)
		initMap(&curMDBEventStats.sizes)

		for i, op := range batch {
			opType := mustExtract[string](op, "op")

			switch opType {
			case "c":
				ops := mustExtract[[]bson.Raw](op, "ops")

				for _, subOp := range ops {
					ns := mustExtract[string](subOp, "ns")
					curStats := lo.Ternary(nsIsInternal(ns), &curMDBEventStats, &curProdEventStats)

					subOpType := mustExtract[string](subOp, "op")
					opType := "applyOps." + subOpType
					curStats.counts[opType]++
					curStats.sizes[opType] += mustExtract[int](subOp, "size")
				}
			case "n":
				// There is nothing to count; this is just here so that
				// reported lag doesn’t balloon in the event of quiesced writes.
			default:
				ns := mustExtract[string](op, "ns")

				curStats := lo.Ternary(nsIsInternal(ns), &curMDBEventStats, &curProdEventStats)

				curStats.counts[opType]++
				curStats.sizes[opType] += mustExtract[int](op, "size")
			}

			if i == len(batch)-1 {
				ts := mustExtract[bson.Timestamp](op, "ts")

				ct := mustExtract[bson.Timestamp](
					cursor.GetExtra()["$clusterTime"].Document(),
					"clusterTime",
				)

				lag.Store(lo.ToPtr(time.Second * time.Duration(int(ct.T)-int(ts.T))))
			}
		}

		prodEventsHistory.Add(curProdEventStats)
		mdbEventsHistory.Add(curMDBEventStats)

		if err := cursor.GetNext(ctx); err != nil {
			return fmt.Errorf("reading oplog: %w", err)
		}
	}

	panic("cursor should never finish!")
}

func nsIsInternal(ns string) bool {
	return strings.HasPrefix(ns, "config.") ||
		strings.HasPrefix(ns, "admin.") ||
		strings.HasPrefix(ns, "local.") ||
		strings.HasPrefix(ns, "__mdb_internal")
}

func _runOplogMode(ctx context.Context, connstr string, interval time.Duration) error {
	client, err := getClient(connstr)
	if err != nil {
		return err
	}

	isSharded, err := isSharded(ctx, client)
	if err != nil {
		return fmt.Errorf("determining whether cluster is sharded: %w", err)
	}

	if isSharded {
		fmt.Println("Connection string refers to a mongos. Will try to connect to individual shards …")

		shardInfos, err := getShards(ctx, client)
		if err != nil {
			return fmt.Errorf("fetching shards’ connection strings: %w", err)
		}

		slices.SortFunc(
			shardInfos,
			func(a, b ShardInfo) int {
				return cmp.Compare(a.Name, b.Name)
			},
		)

		for _, shard := range shardInfos {
			fmt.Printf("Querying shard %#q’s oplog for write stats over the last %s (%s) …\n", shard.Name, interval, shard.ConnStr)

			shardClient, err := getClient(shard.ConnStr)

			if err != nil {
				return fmt.Errorf("creating connection to shard %#q (%#q): %w", shard.Name, shard.ConnStr, err)
			}

			err = fetchAndPrintOplogStats(ctx, shardClient, interval)
			if err != nil {
				return fmt.Errorf("getting shard %s’s oplog stats: %w", shard.Name, err)
			}
		}

		return nil
	}

	fmt.Printf("Querying the oplog for write stats over the last %s …\n", interval)

	return fetchAndPrintOplogStats(ctx, client, interval)
}

func fetchAndPrintOplogStats(ctx context.Context, client *mongo.Client, interval time.Duration) error {
	coll := client.Database("local").Collection("oplog.rs")

	createPipeline := func() mongo.Pipeline {

		// NB: This query seems to run much faster when querying on a full
		// timestamp rather than just ts.t.
		unixTimeStart := uint32(time.Now().Add(-interval).Unix())

		return lo.Flatten([]mongo.Pipeline{
			{
				// Match timestamp
				{{"$match", bson.D{
					{"ts", bson.D{{"$gte", bson.Timestamp{T: unixTimeStart}}}},
				}}},
			},
			oplogUnrollFormatStages,
			{
				// Group by op type
				{{"$group", bson.D{
					{"_id", "$ops.op"},
					{"count", bson.D{{"$sum", 1}}},
					{"totalSize", bson.D{{"$sum", "$size"}}},
					{"minTs", bson.D{{"$min", "$ts"}}},
					{"maxTs", bson.D{{"$max", "$ts"}}},
				}}},

				// Final projection
				{{"$project", bson.D{
					{"op", "$_id"},
					{"count", 1},
					{"totalSize", 1},
					{"minTs", 1},
					{"maxTs", 1},
					{"_id", 0},
				}}},
			},
		})
	}

	cursor, err := coll.Aggregate(ctx, createPipeline())
	if err != nil {
		return fmt.Errorf("querying oplog: %w", err)
	}

	defer cursor.Close(ctx)

	var minTS, maxTS bson.Timestamp
	eventSizesByType := map[string]int{}
	eventCountsByType := map[string]int{}

	for cursor.Next(ctx) {
		decoded := struct {
			Op        string
			Count     int
			TotalSize int
			MinTS     bson.Timestamp
			MaxTS     bson.Timestamp
		}{}

		if err := cursor.Decode(&decoded); err != nil {
			return fmt.Errorf("decoding oplog aggregation: %w", err)
		}

		eventSizesByType[decoded.Op] = decoded.TotalSize
		eventCountsByType[decoded.Op] = decoded.Count

		if minTS.IsZero() {
			minTS = decoded.MinTS
		} else {
			minTS = lo.MinBy(
				sliceOf(minTS, decoded.MinTS),
				bson.Timestamp.Before,
			)
		}

		maxTS = lo.MaxBy(
			sliceOf(maxTS, decoded.MaxTS),
			bson.Timestamp.After,
		)
	}
	if err := cursor.Err(); err != nil {
		return fmt.Errorf("reading oplog aggregation: %w", err)
	}

	delta := time.Duration(1+maxTS.T-minTS.T) * time.Second

	displayTable(
		eventCountsByType,
		eventSizesByType,
		delta,
	)

	return nil
}
