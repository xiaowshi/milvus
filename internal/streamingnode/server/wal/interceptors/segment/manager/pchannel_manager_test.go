package manager

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc"

	"github.com/milvus-io/milvus/internal/mocks/mock_metastore"
	"github.com/milvus-io/milvus/internal/mocks/streamingnode/server/mock_wal"
	"github.com/milvus-io/milvus/internal/streamingnode/server/resource"
	"github.com/milvus-io/milvus/internal/streamingnode/server/wal"
	"github.com/milvus-io/milvus/internal/streamingnode/server/wal/interceptors/segment/inspector"
	"github.com/milvus-io/milvus/internal/streamingnode/server/wal/interceptors/segment/stats"
	"github.com/milvus-io/milvus/internal/streamingnode/server/wal/interceptors/txn"
	internaltypes "github.com/milvus-io/milvus/internal/types"
	"github.com/milvus-io/milvus/internal/util/idalloc"
	"github.com/milvus-io/milvus/pkg/v2/proto/datapb"
	"github.com/milvus-io/milvus/pkg/v2/proto/rootcoordpb"
	"github.com/milvus-io/milvus/pkg/v2/proto/streamingpb"
	"github.com/milvus-io/milvus/pkg/v2/streaming/util/message"
	"github.com/milvus-io/milvus/pkg/v2/streaming/util/types"
	"github.com/milvus-io/milvus/pkg/v2/streaming/walimpls/impls/rmq"
	"github.com/milvus-io/milvus/pkg/v2/util/merr"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v2/util/syncutil"
	"github.com/milvus-io/milvus/pkg/v2/util/tsoutil"
)

func TestSegmentAllocManager(t *testing.T) {
	initializeTestState(t)

	w := mock_wal.NewMockWAL(t)
	w.EXPECT().Append(mock.Anything, mock.Anything).Return(&wal.AppendResult{
		MessageID: rmq.NewRmqID(1),
		TimeTick:  2,
	}, nil)
	f := syncutil.NewFuture[wal.WAL]()
	f.Set(w)

	m, err := RecoverPChannelSegmentAllocManager(context.Background(), types.PChannelInfo{Name: "v1"}, f)
	assert.NoError(t, err)
	assert.NotNil(t, m)

	ctx := context.Background()

	// Ask for a too old timetick.
	result, err := m.AssignSegment(ctx, &AssignSegmentRequest{
		CollectionID: 1,
		PartitionID:  1,
		InsertMetrics: stats.InsertMetrics{
			Rows:       100,
			BinarySize: 100,
		},
		TimeTick: 1,
	})
	assert.Nil(t, result)
	assert.ErrorIs(t, err, ErrTimeTickTooOld)

	// Ask for allocate segment
	result, err = m.AssignSegment(ctx, &AssignSegmentRequest{
		CollectionID: 1,
		PartitionID:  1,
		InsertMetrics: stats.InsertMetrics{
			Rows:       100,
			BinarySize: 100,
		},
		TimeTick: tsoutil.GetCurrentTime(),
	})
	assert.NoError(t, err)
	assert.NotNil(t, result)

	// Ask for allocate more segment, will generated new growing segment.
	result2, err := m.AssignSegment(ctx, &AssignSegmentRequest{
		CollectionID: 1,
		PartitionID:  1,
		InsertMetrics: stats.InsertMetrics{
			Rows:       1024 * 1024,
			BinarySize: 1024 * 1024, // 1MB setting at paramtable.
		},
		TimeTick: tsoutil.GetCurrentTime(),
	})
	assert.NoError(t, err)
	assert.NotNil(t, result2)

	// Ask for seal segment.
	// Here already have a sealed segment, and a growing segment wait for seal, but the result is not acked.
	m.TryToSealSegments(ctx)
	assert.False(t, m.IsNoWaitSeal())

	// The following segment assign will trigger a reach limit, so new seal segment will be created.
	result3, err := m.AssignSegment(ctx, &AssignSegmentRequest{
		CollectionID: 1,
		PartitionID:  1,
		InsertMetrics: stats.InsertMetrics{
			Rows:       1,
			BinarySize: 1,
		},
		TimeTick: tsoutil.GetCurrentTime(),
	})
	assert.NoError(t, err)
	assert.NotNil(t, result3)
	m.TryToSealSegments(ctx)
	assert.False(t, m.IsNoWaitSeal()) // result2 is not acked, so new seal segment will not be sealed right away.

	result.Ack()
	result2.Ack()
	result3.Ack()
	m.TryToSealWaitedSegment(ctx)
	assert.True(t, m.IsNoWaitSeal()) // result2 is acked, so new seal segment will be sealed right away.

	// interactive with txn
	txnManager := txn.NewTxnManager(types.PChannelInfo{Name: "test"}, nil)
	msg := message.NewBeginTxnMessageBuilderV2().
		WithVChannel("v1").
		WithHeader(&message.BeginTxnMessageHeader{KeepaliveMilliseconds: 1000}).
		WithBody(&message.BeginTxnMessageBody{}).
		MustBuildMutable().
		WithTimeTick(tsoutil.GetCurrentTime())

	beginTxnMsg, _ := message.AsMutableBeginTxnMessageV2(msg)
	txn, err := txnManager.BeginNewTxn(ctx, beginTxnMsg)
	assert.NoError(t, err)
	txn.BeginDone()

	for i := 0; i < 3; i++ {
		result, err = m.AssignSegment(ctx, &AssignSegmentRequest{
			CollectionID: 1,
			PartitionID:  1,
			InsertMetrics: stats.InsertMetrics{
				Rows:       1024 * 1024,
				BinarySize: 1024 * 1024, // 1MB setting at paramtable.
			},
			TxnSession: txn,
			TimeTick:   tsoutil.GetCurrentTime(),
		})
		assert.NoError(t, err)
		result.Ack()
	}
	// because of there's a txn session uncommitted, so the segment will not be sealed.
	m.TryToSealSegments(ctx)
	assert.False(t, m.IsNoWaitSeal())

	err = txn.RequestCommitAndWait(context.Background(), 0)
	assert.NoError(t, err)
	txn.CommitDone()
	m.TryToSealSegments(ctx)
	assert.True(t, m.IsNoWaitSeal())

	// Try to seal a partition.
	m.TryToSealSegments(ctx, stats.SegmentBelongs{
		CollectionID: 1,
		VChannel:     "v1",
		PartitionID:  2,
		PChannel:     "v1",
		SegmentID:    3,
	})
	assert.True(t, m.IsNoWaitSeal())

	// Try to seal with a policy
	resource.Resource().SegmentAssignStatsManager().UpdateOnSync(6000, stats.SyncOperationMetrics{
		BinLogCounterIncr: 100,
	})
	// ask a unacknowledgement seal for partition 3 to avoid seal operation.
	result, err = m.AssignSegment(ctx, &AssignSegmentRequest{
		CollectionID: 1,
		PartitionID:  3,
		InsertMetrics: stats.InsertMetrics{
			Rows:       100,
			BinarySize: 100,
		},
		TimeTick: tsoutil.GetCurrentTime(),
	})
	assert.NoError(t, err)
	assert.NotNil(t, result)

	// Should be collected but not sealed.
	m.TryToSealSegments(ctx)
	assert.False(t, m.IsNoWaitSeal())
	result.Ack()
	// Should be sealed.
	m.TryToSealSegments(ctx)
	assert.True(t, m.IsNoWaitSeal())

	// Test fence
	ts := tsoutil.GetCurrentTime()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	ids, err := m.SealAndFenceSegmentUntil(ctx, 1, ts)
	assert.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, ids)
	assert.False(t, m.IsNoWaitSeal())
	m.TryToSealSegments(ctx)
	assert.True(t, m.IsNoWaitSeal())

	result, err = m.AssignSegment(ctx, &AssignSegmentRequest{
		CollectionID: 1,
		PartitionID:  3,
		InsertMetrics: stats.InsertMetrics{
			Rows:       100,
			BinarySize: 100,
		},
		TimeTick: ts,
	})
	assert.ErrorIs(t, err, ErrFencedAssign)
	assert.Nil(t, result)

	m.Close(ctx)
}

func TestCreateAndDropCollection(t *testing.T) {
	initializeTestState(t)

	w := mock_wal.NewMockWAL(t)
	w.EXPECT().Append(mock.Anything, mock.Anything).Return(&wal.AppendResult{
		MessageID: rmq.NewRmqID(1),
		TimeTick:  1,
	}, nil)
	f := syncutil.NewFuture[wal.WAL]()
	f.Set(w)

	m, err := RecoverPChannelSegmentAllocManager(context.Background(), types.PChannelInfo{Name: "v1"}, f)
	assert.NoError(t, err)
	assert.NotNil(t, m)

	m.MustSealSegments(context.Background(), stats.SegmentBelongs{
		PChannel:     "v1",
		VChannel:     "v1",
		CollectionID: 1,
		PartitionID:  2,
		SegmentID:    4000,
	})

	inspector.GetSegmentSealedInspector().RegisterPChannelManager(m)

	ctx := context.Background()

	testRequest := &AssignSegmentRequest{
		CollectionID: 100,
		PartitionID:  101,
		InsertMetrics: stats.InsertMetrics{
			Rows:       100,
			BinarySize: 200,
		},
		TimeTick: tsoutil.GetCurrentTime(),
	}

	resp, err := m.AssignSegment(ctx, testRequest)
	assert.Error(t, err)
	assert.Nil(t, resp)

	m.NewCollection(100, "v1", []int64{101, 102, 103})
	resp, err = m.AssignSegment(ctx, testRequest)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	resp.Ack()

	testRequest.PartitionID = 104
	resp, err = m.AssignSegment(ctx, testRequest)
	assert.Error(t, err)
	assert.Nil(t, resp)

	m.NewPartition(100, 104)
	resp, err = m.AssignSegment(ctx, testRequest)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	resp.Ack()

	m.RemovePartition(ctx, 100, 104)
	assert.True(t, m.IsNoWaitSeal())
	resp, err = m.AssignSegment(ctx, testRequest)
	assert.Error(t, err)
	assert.Nil(t, resp)

	m.RemoveCollection(ctx, 100)
	resp, err = m.AssignSegment(ctx, testRequest)
	assert.True(t, m.IsNoWaitSeal())
	assert.Error(t, err)
	assert.Nil(t, resp)
}

func newStat(insertedBinarySize uint64, maxBinarySize uint64) *streamingpb.SegmentAssignmentStat {
	return &streamingpb.SegmentAssignmentStat{
		MaxBinarySize:         maxBinarySize,
		InsertedRows:          insertedBinarySize,
		InsertedBinarySize:    insertedBinarySize,
		CreateTimestamp:       time.Now().Unix(),
		LastModifiedTimestamp: time.Now().Unix(),
	}
}

// initializeTestState is a helper function to initialize the status for testing.
func initializeTestState(t *testing.T) {
	// c 1
	//		p 1
	//			s 1000p
	//		p 2
	//			s 2000g, 3000g, 4000s, 5000g
	// 		p 3
	//			s 6000g

	paramtable.Init()
	paramtable.Get().DataCoordCfg.SegmentSealProportion.SwapTempValue("1.0")
	paramtable.Get().DataCoordCfg.SegmentSealProportionJitter.SwapTempValue("0.0")
	paramtable.Get().DataCoordCfg.SegmentMaxSize.SwapTempValue("1")
	paramtable.Get().Save(paramtable.Get().CommonCfg.EnableStorageV2.Key, "true")

	streamingNodeCatalog := mock_metastore.NewMockStreamingNodeCataLog(t)

	rootCoordClient := idalloc.NewMockRootCoordClient(t)
	rootCoordClient.EXPECT().AllocSegment(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, asr *datapb.AllocSegmentRequest, co ...grpc.CallOption) (*datapb.AllocSegmentResponse, error) {
		return &datapb.AllocSegmentResponse{
			SegmentInfo: &datapb.SegmentInfo{
				ID:           asr.GetSegmentId(),
				CollectionID: asr.GetCollectionId(),
				PartitionID:  asr.GetPartitionId(),
			},
			Status: merr.Success(),
		}, nil
	})
	rootCoordClient.EXPECT().GetPChannelInfo(mock.Anything, mock.Anything).Return(&rootcoordpb.GetPChannelInfoResponse{
		Collections: []*rootcoordpb.CollectionInfoOnPChannel{
			{
				CollectionId: 1,
				Partitions: []*rootcoordpb.PartitionInfoOnPChannel{
					{PartitionId: 1},
					{PartitionId: 2},
					{PartitionId: 3},
				},
			},
		},
	}, nil)
	fRootCoordClient := syncutil.NewFuture[internaltypes.MixCoordClient]()
	fRootCoordClient.Set(rootCoordClient)

	resource.InitForTest(t,
		resource.OptStreamingNodeCatalog(streamingNodeCatalog),
		resource.OptMixCoordClient(fRootCoordClient),
	)
	streamingNodeCatalog.EXPECT().ListSegmentAssignment(mock.Anything, mock.Anything).Return(
		[]*streamingpb.SegmentAssignmentMeta{
			{
				CollectionId: 1,
				PartitionId:  1,
				SegmentId:    1000,
				Vchannel:     "v1",
				State:        streamingpb.SegmentAssignmentState_SEGMENT_ASSIGNMENT_STATE_PENDING,
				Stat:         nil,
			},
			{
				CollectionId: 1,
				PartitionId:  2,
				SegmentId:    2000,
				Vchannel:     "v1",
				State:        streamingpb.SegmentAssignmentState_SEGMENT_ASSIGNMENT_STATE_GROWING,
				Stat:         newStat(1000, 1000),
			},
			{
				CollectionId: 1,
				PartitionId:  2,
				SegmentId:    3000,
				Vchannel:     "v1",
				State:        streamingpb.SegmentAssignmentState_SEGMENT_ASSIGNMENT_STATE_GROWING,
				Stat:         newStat(100, 1000),
			},
			{
				CollectionId: 1,
				PartitionId:  2,
				SegmentId:    4000,
				Vchannel:     "v1",
				State:        streamingpb.SegmentAssignmentState_SEGMENT_ASSIGNMENT_STATE_SEALED,
				Stat:         newStat(900, 1000),
			},
			{
				CollectionId: 1,
				PartitionId:  2,
				SegmentId:    5000,
				Vchannel:     "v1",
				State:        streamingpb.SegmentAssignmentState_SEGMENT_ASSIGNMENT_STATE_GROWING,
				Stat:         newStat(900, 1000),
			},
			{
				CollectionId: 1,
				PartitionId:  3,
				SegmentId:    6000,
				Vchannel:     "v1",
				State:        streamingpb.SegmentAssignmentState_SEGMENT_ASSIGNMENT_STATE_GROWING,
				Stat:         newStat(100, 1000),
			},
		}, nil)
	streamingNodeCatalog.EXPECT().SaveSegmentAssignments(mock.Anything, mock.Anything, mock.Anything).Return(nil)
}
