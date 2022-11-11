package pipeline

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/streamingfast/bstream"

	pbsubstreams "github.com/streamingfast/substreams/pb/sf/substreams/v1"
)

func Test_resolveStartBlockNum(t *testing.T) {
	tests := []struct {
		name             string
		req              *pbsubstreams.Request
		expectedBlockNum uint64
		wantErr          bool
	}{
		{
			name: "invalid cursor step",
			req: &pbsubstreams.Request{
				StartBlockNum: 10,
				StartCursor: (&bstream.Cursor{
					Step:      bstream.StepType(0),
					Block:     bstream.NewBlockRef("10a", 10),
					LIB:       bstream.NewBlockRef("9a", 9),
					HeadBlock: bstream.NewBlockRef("10a", 10),
				}).ToOpaque(),
			},
			expectedBlockNum: 0,
			wantErr:          true,
		},
		{
			name: "step undo",
			req: &pbsubstreams.Request{
				StartBlockNum: 10,
				StartCursor: (&bstream.Cursor{
					Step:      bstream.StepUndo,
					Block:     bstream.NewBlockRef("10a", 10),
					LIB:       bstream.NewBlockRef("9a", 9),
					HeadBlock: bstream.NewBlockRef("10a", 10),
				}).ToOpaque(),
			},
			expectedBlockNum: 10,
			wantErr:          false,
		},
		{
			name: "step new",
			req: &pbsubstreams.Request{
				StartBlockNum: 10,
				StartCursor: (&bstream.Cursor{
					Step:      bstream.StepNew,
					Block:     bstream.NewBlockRef("10a", 10),
					LIB:       bstream.NewBlockRef("9a", 9),
					HeadBlock: bstream.NewBlockRef("10a", 10),
				}).ToOpaque(),
			},
			expectedBlockNum: 11,
			wantErr:          false,
		},
		{
			name: "step irreversible",
			req: &pbsubstreams.Request{
				StartBlockNum: 10,
				StartCursor: (&bstream.Cursor{
					Step:      bstream.StepIrreversible,
					Block:     bstream.NewBlockRef("10a", 10),
					LIB:       bstream.NewBlockRef("9a", 9),
					HeadBlock: bstream.NewBlockRef("10a", 10),
				}).ToOpaque(),
			},
			expectedBlockNum: 11,
			wantErr:          false,
		},
		{
			name: "step new irreversible",
			req: &pbsubstreams.Request{
				StartBlockNum: 10,
				StartCursor: (&bstream.Cursor{
					Step:      bstream.StepNewIrreversible,
					Block:     bstream.NewBlockRef("10a", 10),
					LIB:       bstream.NewBlockRef("9a", 9),
					HeadBlock: bstream.NewBlockRef("10a", 10),
				}).ToOpaque(),
			},
			expectedBlockNum: 11,
			wantErr:          false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveStartBlockNum(tt.req)
			if (err != nil) != tt.wantErr {
				t.Errorf("resolveStartBlockNum() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.expectedBlockNum {
				t.Errorf("resolveStartBlockNum() got = %v, want %v", got, tt.expectedBlockNum)
			}
		})
	}
}

func Test_computeLiveHandoffBlockNum(t *testing.T) {
	tests := []struct {
		liveHubAvailable bool
		recentBlockNum   uint64
		stopBlockNum     uint64
		expectHandoffNum uint64
		expectError      bool
	}{
		{true, 100, 0, 100, false},
		{true, 100, 150, 100, false},
		{true, 100, 50, 50, false},
		{false, 0, 50, 50, false},
		{false, 0, 0, 0, true},
	}

	for _, test := range tests {
		t.Run("", func(t *testing.T) {
			got, err := computeLiveHandoffBlockNum(func() (uint64, error) {
				if !test.liveHubAvailable {
					return 0, fmt.Errorf("live not available")
				}
				return test.recentBlockNum, nil
			}, test.stopBlockNum)
			if test.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, test.expectHandoffNum, got)
			}
		})
	}
}

func TestBuildRequestDetails(t *testing.T) {
	req, err := BuildRequestDetails(&pbsubstreams.Request{
		StartBlockNum:  10,
		ProductionMode: false,
	}, true, func() (uint64, error) {
		assert.False(t, true, "shouldn't pass here")
		return 999, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 10, int(req.RequestStartBlockNum))
	assert.Equal(t, 10, int(req.LinearHandoffBlockNum))

	req, err = BuildRequestDetails(&pbsubstreams.Request{
		StartBlockNum:  10,
		ProductionMode: true,
	}, true, func() (uint64, error) {
		return 999, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 10, int(req.RequestStartBlockNum))
	assert.Equal(t, 999, int(req.LinearHandoffBlockNum))
}
