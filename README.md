# mongo-speedcam

This tool reports various statistics around a MongoDB cluster’s write load.

## Build & Usage Instructions
```
go build
```
Then type `./mongo-speedcam --help`.

Or, to avoid the build step, do:
```
go run . --help
```

## Oplog Mode

In its simplest form, this tool queries the oplog then prints a report, thus:
```
> ./mongo-speedcam aggregate-oplog 'mongodb://localhost:27017'

Querying the oplog for write stats over the last 1m0s …

23,517.04 ops/sec (15.06 MiB/sec; avg: 671.34 bytes)
┌────────────┬─────────┬───────────┬───────────────────┬──────────────────┐
│ EVENT TYPE │  COUNT  │   SIZE    │ %  OF TOTAL COUNT │ %  OF TOTAL SIZE │
├────────────┼─────────┼───────────┼───────────────────┼──────────────────┤
│ applyOps.d │ 222,911 │ 18.92 MiB │ 21.06%            │ 2.79%            │
│ d          │ 2       │ 256 bytes │ 0%                │ 0%               │
│ i          │ 472,554 │ 596.2 MiB │ 44.65%            │ 87.99%           │
│ u/u        │ 362,800 │ 62.42 MiB │ 34.28%            │ 9.21%            │
└────────────┴─────────┴───────────┴───────────────────┴──────────────────┘
```
`d`, `i`, and `u/u` refer to oplog delete, insert, and update entries,
respectively. (`u/r` indicates a replace operation.) `applyOps.d` indicates
delete entries nested inside `applyOps` entries.

Note that this aggregates entirely on the server. Very little is sent to
this program.

### Tailing

You can also gauge write load by tailing the oplog. This depends in part on
connection speed (because), unlike when aggregating the oplog, here this
program tallies the oplog entries it receives. There is filtering to optimize
the process, but a slow connection may not keep pace regardless.

Because of this issue, a `Lag` is printed with each report in this mode.

### Sharded Clusters

To read from a sharded cluster, this tool will try to connect to each
shard individually and create a report like the above.

This will fail if the tool can’t connect to the shards via the connection
strings that the mongos reports. (This might happen if, for example, your
mongos talks to its shards via private IPs.) In this case, read from the
change stream instead.

(NB: Currently oplog tailing does not work for sharded clusters.)

## Change Stream Mode

This tool can also compile statistics by reading a change stream. This
tends to underperform direct oplog reads but works seamlessly with sharded
clusters.

Like oplog mode, this will compile statistics once then exit.

## Tail Change Stream Mode

You can also tail the change stream and report metrics as they arrive.
In this mode, the tool runs until stopped (e.g., via CTRL-C).

Note that, if the change stream lags the source, the reported write speed
will be lower than reality. For this reason, in this mode the tool also
reports change stream lag.

## Server Status Mode

In this mode, write log stats are gathered by querying server status.
