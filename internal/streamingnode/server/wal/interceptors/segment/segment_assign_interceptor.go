package segment

import (
	"context"
	"time"

	"github.com/cockroachdb/errors"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/streamingnode/server/wal/interceptors"
	"github.com/milvus-io/milvus/internal/streamingnode/server/wal/interceptors/redo"
	"github.com/milvus-io/milvus/internal/streamingnode/server/wal/interceptors/segment/inspector"
	"github.com/milvus-io/milvus/internal/streamingnode/server/wal/interceptors/segment/manager"
	"github.com/milvus-io/milvus/internal/streamingnode/server/wal/interceptors/segment/stats"
	"github.com/milvus-io/milvus/internal/streamingnode/server/wal/interceptors/txn"
	"github.com/milvus-io/milvus/internal/streamingnode/server/wal/utility"
	"github.com/milvus-io/milvus/internal/util/streamingutil/status"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/proto/messagespb"
	"github.com/milvus-io/milvus/pkg/v2/streaming/util/message"
	"github.com/milvus-io/milvus/pkg/v2/util/syncutil"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

const interceptorName = "segment-assign"

var (
	_ interceptors.InterceptorWithMetrics = (*segmentInterceptor)(nil)
	_ interceptors.InterceptorWithReady   = (*segmentInterceptor)(nil)
)

// segmentInterceptor is the implementation of segment assignment interceptor.
type segmentInterceptor struct {
	ctx    context.Context
	cancel context.CancelFunc

	logger        *log.MLogger
	assignManager *syncutil.Future[*manager.PChannelSegmentAllocManager]
}

func (impl *segmentInterceptor) Name() string {
	return interceptorName
}

// Ready returns a channel that will be closed when the segment interceptor is ready.
func (impl *segmentInterceptor) Ready() <-chan struct{} {
	// Wait for segment assignment manager ready.
	return impl.assignManager.Done()
}

// DoAppend assigns segment for every partition in the message.
func (impl *segmentInterceptor) DoAppend(ctx context.Context, msg message.MutableMessage, appendOp interceptors.Append) (msgID message.MessageID, err error) {
	switch msg.MessageType() {
	case message.MessageTypeCreateCollection:
		return impl.handleCreateCollection(ctx, msg, appendOp)
	case message.MessageTypeDropCollection:
		return impl.handleDropCollection(ctx, msg, appendOp)
	case message.MessageTypeCreatePartition:
		return impl.handleCreatePartition(ctx, msg, appendOp)
	case message.MessageTypeDropPartition:
		return impl.handleDropPartition(ctx, msg, appendOp)
	case message.MessageTypeInsert:
		return impl.handleInsertMessage(ctx, msg, appendOp)
	case message.MessageTypeManualFlush:
		return impl.handleManualFlushMessage(ctx, msg, appendOp)
	default:
		return appendOp(ctx, msg)
	}
}

// handleCreateCollection handles the create collection message.
func (impl *segmentInterceptor) handleCreateCollection(ctx context.Context, msg message.MutableMessage, appendOp interceptors.Append) (message.MessageID, error) {
	createCollectionMsg, err := message.AsMutableCreateCollectionMessageV1(msg)
	if err != nil {
		return nil, err
	}
	// send the create collection message.
	msgID, err := appendOp(ctx, msg)
	if err != nil {
		return msgID, err
	}

	// Set up the partition manager for the collection, new incoming insert message can be assign segment.
	h := createCollectionMsg.Header()
	impl.assignManager.Get().NewCollection(h.GetCollectionId(), msg.VChannel(), h.GetPartitionIds())
	return msgID, nil
}

// handleDropCollection handles the drop collection message.
func (impl *segmentInterceptor) handleDropCollection(ctx context.Context, msg message.MutableMessage, appendOp interceptors.Append) (message.MessageID, error) {
	dropCollectionMessage, err := message.AsMutableDropCollectionMessageV1(msg)
	if err != nil {
		return nil, err
	}
	// Drop collections remove all partition managers from assignment service.
	h := dropCollectionMessage.Header()
	if err := impl.assignManager.Get().RemoveCollection(ctx, h.GetCollectionId()); err != nil {
		return nil, err
	}

	// send the drop collection message.
	return appendOp(ctx, msg)
}

// handleCreatePartition handles the create partition message.
func (impl *segmentInterceptor) handleCreatePartition(ctx context.Context, msg message.MutableMessage, appendOp interceptors.Append) (message.MessageID, error) {
	createPartitionMessage, err := message.AsMutableCreatePartitionMessageV1(msg)
	if err != nil {
		return nil, err
	}
	// send the create collection message.
	msgID, err := appendOp(ctx, msg)
	if err != nil {
		return msgID, err
	}

	// Set up the partition manager for the collection, new incoming insert message can be assign segment.
	h := createPartitionMessage.Header()
	// error can never happens for wal lifetime control.
	_ = impl.assignManager.Get().NewPartition(h.GetCollectionId(), h.GetPartitionId())
	return msgID, nil
}

// handleDropPartition handles the drop partition message.
func (impl *segmentInterceptor) handleDropPartition(ctx context.Context, msg message.MutableMessage, appendOp interceptors.Append) (message.MessageID, error) {
	dropPartitionMessage, err := message.AsMutableDropPartitionMessageV1(msg)
	if err != nil {
		return nil, err
	}

	// drop partition, remove the partition manager from assignment service.
	h := dropPartitionMessage.Header()
	if err := impl.assignManager.Get().RemovePartition(ctx, h.GetCollectionId(), h.GetPartitionId()); err != nil {
		return nil, err
	}

	// send the create collection message.
	return appendOp(ctx, msg)
}

// handleInsertMessage handles the insert message.
func (impl *segmentInterceptor) handleInsertMessage(ctx context.Context, msg message.MutableMessage, appendOp interceptors.Append) (message.MessageID, error) {
	insertMsg, err := message.AsMutableInsertMessageV1(msg)
	if err != nil {
		return nil, err
	}
	// Assign segment for insert message.
	// !!! Current implementation a insert message only has one parition, but we need to merge the message for partition-key in future.
	header := insertMsg.Header()
	for _, partition := range header.GetPartitions() {
		result, err := impl.assignManager.Get().AssignSegment(ctx, &manager.AssignSegmentRequest{
			CollectionID: header.GetCollectionId(),
			PartitionID:  partition.GetPartitionId(),
			InsertMetrics: stats.InsertMetrics{
				Rows:       partition.GetRows(),
				BinarySize: uint64(msg.EstimateSize()), // TODO: Use parition.BinarySize in future when merge partitions together in one message.
			},
			TimeTick:   msg.TimeTick(),
			TxnSession: txn.GetTxnSessionFromContext(ctx),
		})
		if errors.Is(err, manager.ErrTimeTickTooOld) {
			// If current time tick of insert message is too old to alloc segment,
			// we just redo it to refresh a new latest timetick.
			return nil, redo.ErrRedo
		}
		if errors.Is(err, manager.ErrTooLargeInsert) {
			// Message is too large, so retry operation is unrecoverable, can't be retry at client side.
			return nil, status.NewUnrecoverableError("insert too large, binary size: %d", msg.EstimateSize())
		}
		if err != nil {
			return nil, err
		}
		// once the segment assignment is done, we need to ack the result,
		// if other partitions failed to assign segment or wal write failure,
		// the segment assignment will not rolled back for simple implementation.
		defer result.Ack()

		// Attach segment assignment to message.
		partition.SegmentAssignment = &message.SegmentAssignment{
			SegmentId: result.SegmentID,
		}
	}
	// Update the insert message headers.
	insertMsg.OverwriteHeader(header)

	return appendOp(ctx, msg)
}

// handleManualFlushMessage handles the manual flush message.
func (impl *segmentInterceptor) handleManualFlushMessage(ctx context.Context, msg message.MutableMessage, appendOp interceptors.Append) (message.MessageID, error) {
	maunalFlushMsg, err := message.AsMutableManualFlushMessageV2(msg)
	if err != nil {
		return nil, err
	}
	header := maunalFlushMsg.Header()
	segmentIDs, err := impl.assignManager.Get().SealAndFenceSegmentUntil(ctx, header.GetCollectionId(), header.GetFlushTs())
	if err != nil {
		return nil, status.NewInner("segment seal failure with error: %s", err.Error())
	}
	// Modify the extra response for manual flush message.
	utility.ModifyAppendResultExtra(ctx, func(old *message.ManualFlushExtraResponse) *message.ManualFlushExtraResponse {
		if old == nil {
			return &messagespb.ManualFlushExtraResponse{SegmentIds: segmentIDs}
		}
		return &messagespb.ManualFlushExtraResponse{SegmentIds: append(old.GetSegmentIds(), segmentIDs...)}
	})
	if len(segmentIDs) > 0 {
		// There's some new segment sealed, we need to retry the manual flush operation refresh the context.
		// If we don't refresh the context, the sequence of message in wal will be:
		// FlushTsHere -> ManualFlush -> FlushSegment1 -> FlushSegment2 -> FlushSegment3.
		// After refresh the context, keep the sequence of the message in the wal with following seq:
		// FlushTsHere -> FlushSegment1 -> FlushSegment2 -> FlushSegment3 -> ManualFlush.
		return nil, redo.ErrRedo
	}

	// send the manual flush message.
	msgID, err := appendOp(ctx, msg)
	if err != nil {
		return nil, err
	}

	return msgID, nil
}

// Close closes the segment interceptor.
func (impl *segmentInterceptor) Close() {
	impl.cancel()
	assignManager := impl.assignManager.Get()
	if assignManager != nil {
		// unregister the pchannels
		inspector.GetSegmentSealedInspector().UnregisterPChannelManager(assignManager)
		assignManager.Close(context.Background())
	}
}

// recoverPChannelManager recovers PChannel Assignment Manager.
func (impl *segmentInterceptor) recoverPChannelManager(param *interceptors.InterceptorBuildParam) {
	timer := typeutil.NewBackoffTimer(typeutil.BackoffTimerConfig{
		Default: time.Second,
		Backoff: typeutil.BackoffConfig{
			InitialInterval: 10 * time.Millisecond,
			Multiplier:      2.0,
			MaxInterval:     time.Second,
		},
	})
	timer.EnableBackoff()
	for counter := 0; ; counter++ {
		pm, err := manager.RecoverPChannelSegmentAllocManager(impl.ctx, param.ChannelInfo, param.WAL)
		if err != nil {
			ch, d := timer.NextTimer()
			impl.logger.Warn("recover PChannel Assignment Manager failed, wait a backoff", zap.Int("retry", counter), zap.Duration("nextRetryInterval", d), zap.Error(err))
			select {
			case <-impl.ctx.Done():
				impl.logger.Info("segment interceptor has been closed", zap.Error(impl.ctx.Err()))
				impl.assignManager.Set(nil)
				return
			case <-ch:
				continue
			}
		}

		// register the manager into inspector, to do the seal asynchronously
		inspector.GetSegmentSealedInspector().RegisterPChannelManager(pm)
		impl.assignManager.Set(pm)
		impl.logger.Info("recover PChannel Assignment Manager success")
		return
	}
}
