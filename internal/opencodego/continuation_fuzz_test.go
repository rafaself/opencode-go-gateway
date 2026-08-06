package opencodego

import (
	"bytes"
	"testing"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

func FuzzContinuationCorrelation(f *testing.F) {
	f.Add([]byte("call-a"), []byte("result"))
	f.Add([]byte("call-世界"), []byte(""))
	f.Fuzz(func(t *testing.T, rawCallID, rawOutput []byte) {
		if len(rawCallID) > 8<<10 || len(rawOutput) > 32<<10 {
			return
		}
		callID := "call-" + string(bytes.ToValidUTF8(rawCallID, []byte("?")))
		if len(callID) > 8<<10 {
			return
		}
		store, err := NewContinuationStore(ContinuationStoreConfig{
			TTL:                 time.Minute,
			ConsumingLeaseTTL:   time.Minute,
			ConsumedGracePeriod: time.Minute,
			MaxRecords:          2,
			MaxBytesPerRecord:   64 << 10,
			MaxAggregateBytes:   128 << 10,
			CleanupInterval:     time.Hour,
			Now:                 func() time.Time { return time.Unix(0, 0).UTC() },
		})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err := store.Save(PendingTurn{
			Provider: "opencodego",
			Model:    DefaultModel,
			ToolCalls: []UpstreamToolCall{{
				CallID:         callID,
				ProviderCallID: callID,
				Kind:           bridge.ToolFunction,
				Name:           "lookup",
				Arguments:      "{}",
			}},
		}); err != nil {
			return
		}
		lease, err := store.Begin([]bridge.ToolResult{{
			CallID: callID,
			Kind:   bridge.ToolFunction,
			Output: string(rawOutput),
		}})
		if err != nil {
			return
		}
		if err := lease.Commit(); err != nil {
			t.Fatalf("committing a successfully correlated result failed: %v", err)
		}
	})
}
