package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/streamingfast/substreams/orchestrator/work"

	"github.com/streamingfast/bstream/hub"
	"github.com/streamingfast/bstream/stream"
	dgrpcserver "github.com/streamingfast/dgrpc/server"
	"github.com/streamingfast/dstore"
	"github.com/streamingfast/substreams/client"
	"github.com/streamingfast/substreams/metrics"
	pbsubstreams "github.com/streamingfast/substreams/pb/sf/substreams/v1"
	"github.com/streamingfast/substreams/pipeline"
	"github.com/streamingfast/substreams/pipeline/execout/cachev1"
	"github.com/streamingfast/substreams/reqctx"
	"github.com/streamingfast/substreams/service/config"
	"github.com/streamingfast/substreams/wasm"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	ttrace "go.opentelemetry.io/otel/trace"
	"go.uber.org/atomic"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpccode "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type Service struct {
	blockType          string
	partialModeEnabled bool
	wasmExtensions     []wasm.WASMExtensioner
	pipelineOptions    []pipeline.PipelineOptioner
	streamFactory      *StreamFactory

	runtimeConfig config.RuntimeConfig

	tracer ttrace.Tracer
	logger *zap.Logger
}

var workerID atomic.Uint64

func New(
	stateStore dstore.Store,
	blockType string,
	parallelSubRequests uint64,
	blockRangeSizeSubRequests uint64,
	substreamsClientConfig *client.SubstreamsClientConfig,
	opts ...Option,
) (s *Service, err error) {

	zlog.Info("creating gprc client factory", zap.Reflect("config", substreamsClientConfig))
	clientFactory := client.NewFactory(substreamsClientConfig)

	runtimeConfig := config.NewRuntimeConfig(
		1000, // overriden by Options
		1000, // overriden by Options
		blockRangeSizeSubRequests,
		parallelSubRequests,
		stateStore,
		func(logger *zap.Logger) work.Worker {
			return work.NewRemoteWorker(clientFactory, logger)
		},
	)
	s = &Service{
		runtimeConfig: runtimeConfig,
		blockType:     blockType,
		tracer:        otel.GetTracerProvider().Tracer("service"),
	}

	zlog.Info("registering substreams metrics")
	metrics.Metricset.Register()

	for _, opt := range opts {
		opt(s)
	}

	return s, nil
}

func (s *Service) BaseStateStore() dstore.Store {
	return s.runtimeConfig.BaseObjectStore
}

func (s *Service) BlockType() string {
	return s.blockType
}

func (s *Service) Register(
	server dgrpcserver.Server,
	mergedBlocksStore dstore.Store,
	forkedBlocksStore dstore.Store,
	forkableHub *hub.ForkableHub,
	logger *zap.Logger) {

	sf := &StreamFactory{
		mergedBlocksStore: mergedBlocksStore,
		forkedBlocksStore: forkedBlocksStore,
		hub:               forkableHub,
	}

	s.streamFactory = sf
	s.logger = logger
	server.RegisterService(func(gs grpc.ServiceRegistrar) {
		pbsubstreams.RegisterStreamServer(gs, s)
	})
}

func (s *Service) Blocks(request *pbsubstreams.Request, streamSrv pbsubstreams.Stream_BlocksServer) (grpcError error) {
	// We keep `err` here as the unaltered error from `blocks` call, this is used in the EndSpan to record the full error
	// and not only the `grpcError` one which is a subset view of the full `err`.
	var err error
	ctx := streamSrv.Context()
	logger := reqctx.Logger(ctx)
	ctx = reqctx.WithTracer(ctx, s.tracer)

	ctx, span := reqctx.WithSpan(ctx, "substream_request")
	defer span.EndWithErr(&err)

	hostname := updateStreamHeadersHostname(streamSrv, logger)
	span.SetAttributes(attribute.String("hostname", hostname))

	// We execute the blocks stream handler and then transform `err` to a gRPC error, keeping both of them.
	// They will be both `nil` if `err` is `nil` itself.
	err = s.blocks(ctx, request, streamSrv)
	grpcError = s.toGRPCError(err)

	if grpcError != nil && status.Code(grpcError) == codes.Internal {
		logger.Info("unexpected termination of stream of blocks", zap.Error(err))
	}

	return grpcError
}

func (s *Service) blocks(ctx context.Context, request *pbsubstreams.Request, streamSrv pbsubstreams.Stream_BlocksServer) error {
	logger := reqctx.Logger(ctx)
	logger.Info("validating request")

	moduleTree, err := pipeline.NewModuleTree(request, s.blockType)
	if err != nil {
		return stream.NewErrInvalidArg(err.Error())
	}

	isSubRequest, err := s.isSubRequest(ctx)
	if err != nil {
		return err
	}
	logger.Debug("set is_subrequest", zap.Bool("is_subrequest", isSubRequest))

	ctx, requestStats := setupRequestStats(ctx, logger, s.runtimeConfig.WithRequestStats, isSubRequest)

	requestDetails, err := pipeline.BuildRequestDetails(request, isSubRequest)
	if err != nil {
		return fmt.Errorf("build request details: %w", err)
	}
	ctx = reqctx.WithRequest(ctx, requestDetails)

	if err := moduleTree.ValidateEffectiveStartBlock(requestDetails.EffectiveStartBlockNum); err != nil {
		return stream.NewErrInvalidArg(err.Error())
	}

	execOutputCacheEngine, err := cachev1.NewEngine(s.runtimeConfig, s.blockType, reqctx.Logger(ctx))
	if err != nil {
		return fmt.Errorf("error building caching engine: %w", err)
	}

	wasmRuntime := wasm.NewRuntime(s.wasmExtensions)

	storeConfigs, err := pipeline.InitializeStoreConfigs(moduleTree, s.runtimeConfig.BaseObjectStore)
	if err != nil {
		return fmt.Errorf("configuring stores: %w", err)
	}
	stores := pipeline.NewStores(storeConfigs, s.runtimeConfig.StoreSnapshotsSaveInterval, requestDetails.EffectiveStartBlockNum, request.StopBlockNum, isSubRequest)

	respFunc := responseHandler(logger, streamSrv)
	opts := s.buildPipelineOptions(ctx, request)
	pipe := pipeline.New(
		ctx,
		moduleTree,
		stores,
		wasmRuntime,
		execOutputCacheEngine,
		s.runtimeConfig,
		respFunc,
		opts...,
	)

	if requestStats != nil {
		requestStats.Start(10 * time.Second)
		defer requestStats.Shutdown()
	}
	logger.Info("initializing pipeline",
		zap.Int64("requested_start_block", request.StartBlockNum),
		zap.Uint64("effective_start_block", requestDetails.EffectiveStartBlockNum),
		zap.Uint64("requested_stop_block", request.StopBlockNum),
		zap.String("requested_start_cursor", request.StartCursor),
		zap.Bool("is_back_processing", requestDetails.IsSubRequest),
		zap.Strings("outputs", request.OutputModules),
	)
	if err := pipe.Init(ctx); err != nil {
		return fmt.Errorf("error building pipeline: %w", err)
	}

	// It's ok to use `StartBlockNum` directly here (instead of `requestCtx.EffectiveStartBlockNum`)
	// and in the constructor we also pass `StartCursor` which will be handled by `streamFactory.New`
	// and will be used to bootstrap the stream correctly from it if set.
	logger.Info("creating firehose stream",
		zap.Int64("start_block", request.StartBlockNum),
		zap.Uint64("end_block", request.StopBlockNum),
		zap.String("cursor", request.StartCursor),
	)
	blockStream, err := s.streamFactory.New(
		pipe,
		request.StartBlockNum,
		request.StopBlockNum,
		request.StartCursor,
	)
	if err != nil {
		return fmt.Errorf("error getting stream: %w", err)
	}

	return pipe.Launch(ctx, blockStream, streamSrv)
}

func (s *Service) buildPipelineOptions(ctx context.Context, request *pbsubstreams.Request) (opts []pipeline.Option) {
	for _, pipeOpts := range s.pipelineOptions {
		for _, opt := range pipeOpts.PipelineOptions(ctx, request) {
			opts = append(opts, opt)
		}
	}
	return
}

func responseHandler(logger *zap.Logger, streamSrv pbsubstreams.Stream_BlocksServer) func(resp *pbsubstreams.Response) error {
	return func(resp *pbsubstreams.Response) error {
		if err := streamSrv.Send(resp); err != nil {
			logger.Info("unable to send block probably due to client disconnecting", zap.Error(err))
			return status.Error(codes.Unavailable, err.Error())
		}
		return nil
	}
}

func setupRequestStats(ctx context.Context, logger *zap.Logger, withRequestStats, isSubRequest bool) (context.Context, metrics.Stats) {
	if isSubRequest {
		wid := workerID.Inc()
		logger = logger.With(zap.Uint64("worker_id", wid))
		return reqctx.WithLogger(ctx, logger), metrics.NewNoopStats()
	}

	// we only want to meaure stats when enabled an on the Main request
	if withRequestStats {
		stats := metrics.NewReqStats(logger)
		return reqctx.WithReqStats(ctx, stats), stats
	}
	return ctx, metrics.NewNoopStats()
}

func (s *Service) isSubRequest(ctx context.Context) (bool, error) {
	/*
		this entire `if` is not good, the ctx is from the StreamServer so there
		is no substreams-partial-mode, the actual flag is substreams-partial-mode-enabled
	*/
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		partialMode := md.Get("substreams-partial-mode")
		if len(partialMode) == 1 && partialMode[0] == "true" {
			if !s.partialModeEnabled {
				return false, status.Error(grpccode.InvalidArgument, "substreams-partial-mode not enabled on this instance")
			}
			return true, nil
		}
	}
	return false, nil
}

func updateStreamHeadersHostname(streamSrv pbsubstreams.Stream_BlocksServer, logger *zap.Logger) string {
	hostname, err := os.Hostname()
	if err != nil {
		logger.Warn("cannot find hostname, using 'unknown'", zap.Error(err))
		hostname = "unknown host"
	}
	if os.Getenv("SUBSTREAMS_SEND_HOSTNAME") == "true" {
		md := metadata.New(map[string]string{"host": hostname})
		err = streamSrv.SetHeader(md)
		if err != nil {
			logger.Warn("cannot send header metadata", zap.Error(err))
		}
	}
	return hostname
}

// toGRPCError turns an `err` into a gRPC error if it's non-nil, in the `nil` case,
// `nil` is returned right away.
//
// If the `err` has in its chain of error either `context.Canceled`, `context.DeadlineExceeded`
// or `stream.ErrInvalidArg`, error is turned into a proper gRPC error respectively of code
// `Canceled`, `DeadlineExceeded` or `InvalidArgument`.
//
// If the `err` has its in chain any error constructed through `status.Error` (and its variants), then
// we return the first found error of such type directly, because it's already a gRPC error.
//
// Otherwise, the error is assumed to be an internal error and turned backed into a proper
// `status.Error(codes.Internal, err.Error())`.
func (s *Service) toGRPCError(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "source canceled")
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "source deadline exceeded")
	}

	var errInvalidArg *stream.ErrInvalidArg
	if errors.As(err, &errInvalidArg) {
		return status.Error(codes.InvalidArgument, errInvalidArg.Error())
	}

	// Do we want to print the full cause as coming from Golang? Would we like to maybe trim off "operational"
	// data?
	return status.Error(codes.Internal, err.Error())
}
