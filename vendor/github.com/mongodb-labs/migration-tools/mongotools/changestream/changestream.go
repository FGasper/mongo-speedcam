package changestream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/mongodb-labs/migration-tools/contextplus"
	"github.com/mongodb-labs/migration-tools/future"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	tokenKeyStringField = "__tokenKeyString"
)

var (
	closeCalledErr = fmt.Errorf("close called on ParallelChangeStream")
)

type Options struct {
	Pipeline mongo.Pipeline
	Streams  int
	Options  options.Lister[options.ChangeStreamOptions]
}

type Watcher interface {
	Watch(ctx context.Context, pipeline any,
		opts ...options.Lister[options.ChangeStreamOptions],
	) (*mongo.ChangeStream, error)
}

var _ Watcher = (*mongo.Collection)(nil)

type collLike interface {
	Database() *mongo.Database
}

type dbLike interface {
	Client() *mongo.Client
}

type EventsBatch struct {
	Events        []bson.Raw
	OperationTime bson.Timestamp
	ClusterTime   bson.Raw
}

func NewParallel(
	ctxIn context.Context,
	watcher Watcher,
	opts Options,
) (*ParallelChangeStream, error) {
	if opts.Streams <= 0 {
		return nil, fmt.Errorf("streams (%d) must be positive", opts.Streams)
	}

	createPipeline := func(threadNum int) mongo.Pipeline {
		return lo.Concat(
			mongo.Pipeline{
				{
					{"$match", bson.D{
						{"$expr", bson.D{
							{"$eq", bson.A{
								threadNum,
								bson.D{{"$abs", bson.D{
									{"$mod", bson.A{
										bson.D{{"$toHashedIndexKey", bson.D{
											{"$_internalKeyStringValue", bson.D{
												{"input", "$documentKey._id"},
											}},
										}}},
										opts.Streams,
									}},
								}}},
							}},
						}},
					}},
				},
			},
			opts.Pipeline,
			mongo.Pipeline{
				{
					{"$addFields", bson.D{
						{tokenKeyStringField, bson.D{
							{"$_internalKeyStringValue", bson.D{
								{"input", "$_id"},
							}},
						}},
					}},
				},
			},
		)
	}

	wg := &sync.WaitGroup{}

	errFuture, errSetter := future.New[error]()
	errIsSet := &atomic.Bool{}
	setErr := func(err error) {
		if errIsSet.CompareAndSwap(false, true) {
			errSetter(err)
		}
	}

	channels := make([]chan EventsBatch, opts.Streams)

	ctx, canceler := contextplus.WithCancelCause(ctxIn)

	var client *mongo.Client

	switch w := watcher.(type) {
	case collLike:
		client = w.Database().Client()
	case dbLike:
		client = w.Client()
	case *mongo.Client:
		client = w
	default:
		panic(fmt.Sprintf("watcher type (%T) is unexpected", watcher))
	}

	for threadNum := range opts.Streams {
		curChan := make(chan EventsBatch, 10)
		channels[threadNum] = curChan

		wg.Go(func() {
			defer close(curChan)

			pl := createPipeline(threadNum)

			csOpts := opts.Options
			if csOpts == nil {
				csOpts = options.ChangeStream()
			}

			sess, err := client.StartSession(options.Session().SetCausalConsistency(true))
			if err != nil {
				setErr(fmt.Errorf("start session for thread %d: %w", threadNum, err))
				return
			}
			sctx := mongo.NewSessionContext(ctx, sess)
			defer sess.EndSession(sctx)

			cs, err := watcher.Watch(sctx, pl, csOpts)
			if err != nil {
				setErr(fmt.Errorf("watch change stream for thread %d: %w", threadNum, err))
				return
			}

			defer cs.Close(sctx)

			var events []bson.Raw
			for {
				if !cs.TryNext(sctx) {
					if err := cs.Err(); err != nil {
						setErr(fmt.Errorf("change stream error for thread %d: %w", threadNum, err))
						return
					}

					select {
					case <-sctx.Done():
						setErr(sctx.Err())
						return
					case curChan <- EventsBatch{
						OperationTime: *sess.OperationTime(),
						ClusterTime:   sess.ClusterTime(),
					}:
					}

					continue
				}

				events = append(events, cs.Current)

				if cs.RemainingBatchLength() == 0 {
					select {
					case <-sctx.Done():
						setErr(sctx.Err())
						return
					case curChan <- EventsBatch{
						Events:        slices.Clone(events),
						OperationTime: *sess.OperationTime(),
						ClusterTime:   sess.ClusterTime(),
					}:
					}

					clear(events)
					events = events[:0]
				}
			}
		})
	}

	return &ParallelChangeStream{
		channels:     channels,
		curChanBatch: make([]EventsBatch, opts.Streams),
		errFuture:    errFuture,
		canceler:     canceler,
	}, nil
}

type ParallelChangeStream struct {
	channels     []chan EventsBatch
	curChanBatch []EventsBatch
	errFuture    *future.Future[error]
	nextErr      error
	current      bson.Raw
	canceler     context.CancelCauseFunc
}

func (pcs *ParallelChangeStream) Next(ctx context.Context) bool {
	return pcs.next(ctx, true)
}

func (pcs *ParallelChangeStream) TryNext(ctx context.Context) bool {
	return pcs.next(ctx, false)
}

func (pcs *ParallelChangeStream) next(ctx context.Context, blocking bool) bool {
	chanToken := make([][]byte, len(pcs.channels))

	chansWithEvents := make([]int, 0, len(pcs.channels))

	sess := mongo.SessionFromContext(ctx)

	for i, batch := range pcs.curChanBatch {
		for len(batch.Events) == 0 {
			var ok bool

			select {
			case <-ctx.Done():
				pcs.nextErr = ctx.Err()
				pcs.canceler(pcs.nextErr)
				return false
			case batch, ok = <-pcs.channels[i]:
				if !ok {
					pcs.nextErr = fmt.Errorf("channel %d closed unexpectedly", i)
					pcs.canceler(pcs.nextErr)
					return false
				}

				if sess != nil {
					sess.AdvanceOperationTime(&batch.OperationTime)
					sess.AdvanceClusterTime(batch.ClusterTime)
				}

				pcs.curChanBatch[i] = batch
			}

			if !blocking && len(batch.Events) == 0 {
				// We got an empty batch, so we know this reader has no events.
				// Thus we continue.
				break
			}
		}

		if len(batch.Events) == 0 {
			if blocking {
				panic("blocking but no events available")
			}

			// This reader has no events, so we continue.
			continue
		}

		chansWithEvents = append(chansWithEvents, i)

		token, err := bsontools.RawLookup[bson.Binary](batch.Events[0], tokenKeyStringField)
		if err != nil {
			pcs.nextErr = fmt.Errorf("lookup event token for thread %d: %w", i, err)
			pcs.canceler(pcs.nextErr)
			return false
		}

		chanToken[i] = token.Data
	}

	if len(chansWithEvents) == 0 {
		if blocking {
			panic("blocking but no events available")
		}

		return false
	}

	nextChan := lo.MinBy(
		chansWithEvents,
		func(i, j int) bool {
			return bytes.Compare(chanToken[i], chanToken[j]) < 0
		},
	)

	var found bool
	var err error
	pcs.current, found, err = bsontools.RemoveFromRaw(
		pcs.curChanBatch[nextChan].Events[0],
		tokenKeyStringField,
	)
	if !found {
		panic("no token key string field in change event??")
	}
	if err != nil {
		pcs.nextErr = fmt.Errorf("remove token key string field from change event for thread %d: %w", nextChan, err)
		pcs.canceler(pcs.nextErr)
		return false
	}

	pcs.curChanBatch[nextChan].Events = pcs.curChanBatch[nextChan].Events[1:]

	return true
}

func (pcs *ParallelChangeStream) Current() bson.Raw {
	return pcs.current
}

func (pcs *ParallelChangeStream) Close() {
	pcs.canceler(closeCalledErr)
}

func (pcs *ParallelChangeStream) Err() error {
	nextErr := pcs.nextErr

	var threadErr error

	select {
	case <-pcs.errFuture.Ready():
		threadErr = pcs.errFuture.Get()

		// Ignore the error if it was caused by Close() being called.
		if errors.Is(threadErr, closeCalledErr) {
			threadErr = nil
		}
	default:
	}

	if nextErr != nil {
		if threadErr != nil {
			return fmt.Errorf("thread error: %w; iteration error: %w", threadErr, nextErr)
		}

		return nextErr
	}

	return threadErr
}
