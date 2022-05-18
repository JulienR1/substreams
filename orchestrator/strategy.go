package orchestrator

import (
	"context"
	"fmt"
	"github.com/streamingfast/substreams/state"
	"go.uber.org/zap"

	pbsubstreams "github.com/streamingfast/substreams/pb/sf/substreams/v1"
)

type Strategy interface {
	GetNextRequest() (*pbsubstreams.Request, error)
}

type LinearStrategy struct {
	requests []*pbsubstreams.Request
}

func NewLinearStrategy(ctx context.Context, request *pbsubstreams.Request, builders []*state.Builder, upToBlockNum uint64) (*LinearStrategy, error) {
	res := &LinearStrategy{}

	for _, builder := range builders {
		zlog.Debug("builders", zap.String("builder", builder.Name))
		zlog.Debug("up to block num", zap.Uint64("up_to_block_num", upToBlockNum))
		if upToBlockNum == builder.ModuleStartBlock {
			continue // nothing to synchronize
		}

		endBlock := upToBlockNum
		info, err := builder.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("getting builder info: %w", err)
		}

		lastExclusiveEndBlock := info.LastKVSavedBlock
		zlog.Debug("got info", zap.Object("builder", builder), zap.Uint64("up_to_block", upToBlockNum), zap.Uint64("end_block", lastExclusiveEndBlock))
		if upToBlockNum <= lastExclusiveEndBlock {
			zlog.Debug("no request created", zap.Uint64("up_to_block", upToBlockNum), zap.Uint64("last_exclusive_end_block", lastExclusiveEndBlock))
			continue // not sure if we should pop here
		}

		reqStartBlock := lastExclusiveEndBlock
		if reqStartBlock == 0 {
			reqStartBlock = builder.ModuleStartBlock
		}

		req := createRequest(reqStartBlock, endBlock, builder.Name, request.ForkSteps, request.IrreversibilityCondition, request.Manifest)
		res.requests = append(res.requests, req)
	}

	return res, nil
}

func (s *LinearStrategy) GetNextRequest() (*pbsubstreams.Request, error) {
	if len(s.requests) == 0 {
		return nil, fmt.Errorf("no requests to fetch")
	}

	var request *pbsubstreams.Request
	request, s.requests = s.requests[len(s.requests)-1], s.requests[:len(s.requests)-1]

	return request, nil
}
