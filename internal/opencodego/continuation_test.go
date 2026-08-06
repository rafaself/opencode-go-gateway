package opencodego

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

type continuationTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *continuationTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *continuationTestClock) Advance(delta time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(delta)
	clock.mu.Unlock()
}

func newTestContinuationStore(t *testing.T, clock *continuationTestClock, mutate func(*ContinuationStoreConfig)) *ContinuationStore {
	t.Helper()
	config := ContinuationStoreConfig{
		TTL:                 time.Minute,
		ConsumedGracePeriod: time.Second,
		MaxRecords:          8,
		MaxBytesPerRecord:   1 << 20,
		MaxAggregateBytes:   2 << 20,
		CleanupInterval:     time.Hour,
		Now:                 clock.Now,
	}
	if mutate != nil {
		mutate(&config)
	}
	store, err := NewContinuationStore(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testPendingTurn(callIDs ...string) PendingTurn {
	calls := make([]UpstreamToolCall, 0, len(callIDs))
	for index, callID := range callIDs {
		calls = append(calls, UpstreamToolCall{
			CallID:         callID,
			ProviderCallID: "provider-" + callID,
			Kind:           bridge.ToolFunction,
			Name:           "tool-" + string(rune('a'+index)),
			Arguments:      `{"index":` + string(rune('0'+index)) + `}`,
		})
	}
	return PendingTurn{
		Provider:         ProviderName,
		Model:            DefaultModel,
		ReasoningContent: "private reasoning",
		AssistantContent: "",
		ToolCalls:        calls,
	}
}

func TestContinuationStoreGroupsParallelResultsAndCommitsAfterAcceptance(t *testing.T) {
	clock := &continuationTestClock{now: time.Unix(1700000000, 0).UTC()}
	store := newTestContinuationStore(t, clock, nil)
	if err := store.Save(testPendingTurn("one", "two", "three")); err != nil {
		t.Fatal(err)
	}
	results := []bridge.ToolResult{
		{CallID: "two", Kind: bridge.ToolFunction, Output: "result two"},
		{CallID: "one", Kind: bridge.ToolFunction, Output: "result one", Error: true},
		{CallID: "three", Kind: bridge.ToolFunction, Output: "result three"},
	}
	lease, err := store.Begin(results)
	if err != nil {
		t.Fatal(err)
	}
	if got := lease.Turn().Status; got != PendingStatusConsuming {
		t.Fatalf("lease status = %q", got)
	}
	if got := lease.Results(); len(got) != 3 || got[0].CallID != "two" || !got[1].Error {
		t.Fatalf("lease results = %#v", got)
	}
	if err := lease.Commit(); err != nil {
		t.Fatal(err)
	}
	turn, err := store.Lookup("one")
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != PendingStatusConsumed {
		t.Fatalf("stored status = %q", turn.Status)
	}
	if _, err := store.Begin(results); !errors.Is(err, ErrContinuationConsumed) {
		t.Fatalf("duplicate after commit = %v", err)
	}
	clock.Advance(2 * time.Second)
	if _, err := store.Lookup("one"); !errors.Is(err, ErrContinuationUnknown) || store.RecordCount() != 0 {
		t.Fatalf("consumed grace cleanup = err %v, records %d", err, store.RecordCount())
	}
}

func TestContinuationStoreRejectsUnknownMixedIncompleteDuplicateAndMismatchedResults(t *testing.T) {
	clock := &continuationTestClock{now: time.Unix(1700000000, 0).UTC()}
	store := newTestContinuationStore(t, clock, nil)
	if err := store.Save(testPendingTurn("one", "two")); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(testPendingTurn("other")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		results []bridge.ToolResult
		want    error
	}{
		{name: "unknown", results: []bridge.ToolResult{{CallID: "missing", Kind: bridge.ToolFunction}}, want: ErrContinuationUnknown},
		{name: "incomplete", results: []bridge.ToolResult{{CallID: "one", Kind: bridge.ToolFunction}}, want: ErrContinuationIncomplete},
		{name: "duplicate", results: []bridge.ToolResult{{CallID: "one", Kind: bridge.ToolFunction}, {CallID: "one", Kind: bridge.ToolFunction}}, want: ErrContinuationDuplicate},
		{name: "mixed", results: []bridge.ToolResult{{CallID: "one", Kind: bridge.ToolFunction}, {CallID: "other", Kind: bridge.ToolFunction}}, want: ErrContinuationMixed},
		{name: "mismatched", results: []bridge.ToolResult{{CallID: "one", Kind: bridge.ToolCustom}, {CallID: "two", Kind: bridge.ToolFunction}}, want: ErrContinuationKindMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.Begin(test.results); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestContinuationStoreAbortAllowsRetryAfterCancellationOrUpstream400(t *testing.T) {
	clock := &continuationTestClock{now: time.Unix(1700000000, 0).UTC()}
	store := newTestContinuationStore(t, clock, nil)
	if err := store.Save(testPendingTurn("one")); err != nil {
		t.Fatal(err)
	}
	results := []bridge.ToolResult{{CallID: "one", Kind: bridge.ToolFunction, Output: "retryable"}}
	first, err := store.Begin(results)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Abort(); err != nil {
		t.Fatal(err)
	}
	second, err := store.Begin(results)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestContinuationStoreActiveLeaseOutlivesPendingTTLButHasBoundedCleanup(t *testing.T) {
	clock := &continuationTestClock{now: time.Unix(1700000000, 0).UTC()}
	store := newTestContinuationStore(t, clock, func(config *ContinuationStoreConfig) {
		config.TTL = 5 * time.Second
		config.ConsumingLeaseTTL = 10 * time.Second
		config.ConsumedGracePeriod = 2 * time.Second
	})
	if err := store.Save(testPendingTurn("active")); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Begin([]bridge.ToolResult{{CallID: "active", Kind: bridge.ToolFunction}})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(6 * time.Second)
	if got := store.RecordCount(); got != 1 {
		t.Fatalf("active lease was reclaimed after pending TTL: records = %d", got)
	}
	if err := lease.Commit(); err != nil {
		t.Fatalf("active lease did not commit after pending TTL: %v", err)
	}
	if got := store.RecordCount(); got != 1 {
		t.Fatalf("committed lease was reclaimed before consumed grace: records = %d", got)
	}
	clock.Advance(3 * time.Second)
	if got := store.RecordCount(); got != 0 {
		t.Fatalf("consumed lease was retained after grace: records = %d", got)
	}
}

func TestContinuationStoreExpiresActiveLeaseAtBoundedDeadline(t *testing.T) {
	clock := &continuationTestClock{now: time.Unix(1700000000, 0).UTC()}
	store := newTestContinuationStore(t, clock, func(config *ContinuationStoreConfig) {
		config.TTL = 5 * time.Second
		config.ConsumingLeaseTTL = 3 * time.Second
	})
	if err := store.Save(testPendingTurn("expiring")); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Begin([]bridge.ToolResult{{CallID: "expiring", Kind: bridge.ToolFunction}})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(4 * time.Second)
	if got := store.RecordCount(); got != 0 {
		t.Fatalf("expired active lease was retained: records = %d", got)
	}
	if err := lease.Commit(); !errors.Is(err, ErrContinuationExpired) {
		t.Fatalf("commit after active lease expiry = %v, want %v", err, ErrContinuationExpired)
	}
	if err := lease.Abort(); !errors.Is(err, ErrContinuationFinalized) {
		t.Fatalf("abort after failed commit = %v, want %v", err, ErrContinuationFinalized)
	}
}

func TestContinuationStoreExpiresAndHonorsCapacityLimits(t *testing.T) {
	clock := &continuationTestClock{now: time.Unix(1700000000, 0).UTC()}
	store := newTestContinuationStore(t, clock, func(config *ContinuationStoreConfig) {
		config.TTL = 5 * time.Second
		config.MaxRecords = 1
	})
	if err := store.Save(testPendingTurn("one")); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(testPendingTurn("two")); !errors.Is(err, ErrContinuationCapacity) {
		t.Fatalf("capacity error = %v", err)
	}
	clock.Advance(6 * time.Second)
	if err := store.Save(testPendingTurn("two")); err != nil {
		t.Fatalf("expired state did not release capacity: %v", err)
	}
	if _, err := store.Lookup("one"); !errors.Is(err, ErrContinuationExpired) {
		t.Fatalf("expired lookup = %v", err)
	}
	if got := store.RecordCount(); got != 1 {
		t.Fatalf("record count after reclaim = %d, want 1", got)
	}
}

func TestContinuationStoreEnforcesPerRecordAndAggregateByteLimits(t *testing.T) {
	clock := &continuationTestClock{now: time.Unix(1700000000, 0).UTC()}
	probe := testPendingTurn("probe")
	encoded, err := json.Marshal(probe)
	if err != nil {
		t.Fatal(err)
	}
	perRecordStore := newTestContinuationStore(t, clock, func(config *ContinuationStoreConfig) {
		config.MaxBytesPerRecord = int64(len(encoded) - 1)
	})
	if err := perRecordStore.Save(probe); !errors.Is(err, ErrContinuationCapacity) {
		t.Fatalf("per-record capacity error = %v", err)
	}
	aggregateStore := newTestContinuationStore(t, clock, func(config *ContinuationStoreConfig) {
		config.MaxAggregateBytes = int64(len(encoded) - 1)
	})
	if err := aggregateStore.Save(probe); !errors.Is(err, ErrContinuationCapacity) {
		t.Fatalf("aggregate capacity error = %v", err)
	}
}

func TestContinuationStoreRejectsOversizedPendingStateBeforeCloning(t *testing.T) {
	clock := &continuationTestClock{now: time.Unix(1700000000, 0).UTC()}
	store := newTestContinuationStore(t, clock, func(config *ContinuationStoreConfig) {
		config.MaxBytesPerRecord = 64
		config.MaxAggregateBytes = 64
	})
	turn := testPendingTurn("oversized")
	turn.ToolCalls[0].Arguments = string(make([]byte, 1<<20))
	if err := store.Save(turn); !errors.Is(err, ErrContinuationCapacity) {
		t.Fatalf("oversized pending state error = %v, want %v", err, ErrContinuationCapacity)
	}
	if store.RecordCount() != 0 || store.Bytes() != 0 {
		t.Fatalf("oversized state changed retained metrics: records=%d bytes=%d", store.RecordCount(), store.Bytes())
	}
}

func TestContinuationStoreContextGatesRetentionAndCommit(t *testing.T) {
	clock := &continuationTestClock{now: time.Unix(1700000000, 0).UTC()}
	store := newTestContinuationStore(t, clock, nil)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.SaveContext(canceled, testPendingTurn("canceled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled save error = %v, want %v", err, context.Canceled)
	}
	if store.RecordCount() != 0 {
		t.Fatalf("canceled save retained %d records", store.RecordCount())
	}
	if err := store.Save(testPendingTurn("commit")); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Begin([]bridge.ToolResult{{CallID: "commit", Kind: bridge.ToolFunction}})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.CommitContext(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled commit error = %v, want %v", err, context.Canceled)
	}
	if err := lease.Abort(); err != nil {
		t.Fatalf("aborting gated lease: %v", err)
	}
}

func TestContinuationStoreConcurrentDuplicateSubmissionIsDeterministic(t *testing.T) {
	clock := &continuationTestClock{now: time.Unix(1700000000, 0).UTC()}
	store := newTestContinuationStore(t, clock, nil)
	if err := store.Save(testPendingTurn("one")); err != nil {
		t.Fatal(err)
	}
	results := []bridge.ToolResult{{CallID: "one", Kind: bridge.ToolFunction}}
	first, err := store.Begin(results)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Begin(results); !errors.Is(err, ErrContinuationBusy) {
		t.Fatalf("concurrent duplicate error = %v", err)
	}
	if err := first.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestMapRequestReconstructsOneAssistantTurnAndPreservesCustomWrapper(t *testing.T) {
	clock := &continuationTestClock{now: time.Unix(1700000000, 0).UTC()}
	store := newTestContinuationStore(t, clock, nil)
	turn := PendingTurn{
		Provider:         ProviderName,
		Model:            DefaultModel,
		ReasoningContent: "private reasoning",
		AssistantContent: "",
		ToolCalls: []UpstreamToolCall{
			{CallID: "function-call", ProviderCallID: "provider-function", Kind: bridge.ToolFunction, Name: "lookup", Arguments: `{"query":"one"}`},
			{CallID: "custom-call", ProviderCallID: "provider-custom", Kind: bridge.ToolCustom, Name: ApplyPatchUpstreamName, Arguments: `{"input":"patch"}`, Registration: bridge.ToolRegistration{Kind: bridge.ToolCustom, InboundName: ApplyPatchToolName, UpstreamName: ApplyPatchUpstreamName, WrapperField: ApplyPatchWrapperField}},
		},
	}
	if err := store.Save(turn); err != nil {
		t.Fatal(err)
	}
	results := []bridge.ToolResult{
		{CallID: "custom-call", Kind: bridge.ToolCustom, Output: "custom result"},
		{CallID: "function-call", Kind: bridge.ToolFunction, Output: "function result"},
	}
	lease, err := store.Begin(results)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Abort()
	request := bridge.Request{
		Input: []bridge.InputItem{
			bridge.Message{Role: bridge.RoleUser, Content: []bridge.ContentPart{bridge.TextContent{Text: "continue"}}},
			bridge.FunctionCall{CallID: "function-call", Name: "lookup", Arguments: `{"query":"one"}`},
			bridge.CustomToolCall{CallID: "custom-call", Name: ApplyPatchToolName, Input: "patch"},
			bridge.FunctionCallOutput{CallID: "function-call", Output: "function result"},
			bridge.CustomToolCallOutput{CallID: "custom-call", Output: "custom result"},
		},
		Tools:        []bridge.Tool{bridge.FunctionTool{Name: "lookup", Parameters: mustSchema(t, `{"type":"object"}`)}},
		Generation:   bridge.GenerationOptions{Stream: true, ParallelToolCalls: true},
		Continuation: NewContinuationRequest(lease),
	}
	registry, err := NewToolRegistry(request)
	if err != nil {
		t.Fatal(err)
	}
	request.ToolRegistry = registry
	mapped, err := MapRequest(request, DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped.Messages) != 4 {
		t.Fatalf("messages = %#v", mapped.Messages)
	}
	assistant := mapped.Messages[1]
	if assistant.Role != "assistant" || assistant.Content == nil || *assistant.Content != "" || assistant.ReasoningContent == nil || *assistant.ReasoningContent != "private reasoning" || len(assistant.ToolCalls) != 2 {
		t.Fatalf("assistant replay = %#v", assistant)
	}
	if assistant.ToolCalls[0].ID != "provider-function" || assistant.ToolCalls[0].Function.Name != "lookup" || assistant.ToolCalls[1].ID != "provider-custom" || assistant.ToolCalls[1].Function.Name != ApplyPatchUpstreamName {
		t.Fatalf("tool call order = %#v", assistant.ToolCalls)
	}
	if mapped.Messages[2].ToolCallID != "provider-custom" || mapped.Messages[2].Content == nil || *mapped.Messages[2].Content != "custom result" || mapped.Messages[3].ToolCallID != "provider-function" {
		t.Fatalf("result order = %#v", mapped.Messages[2:])
	}
}

func TestMapRequestCorrelatesCustomContinuationBySemanticInput(t *testing.T) {
	clock := &continuationTestClock{now: time.Unix(1700000000, 0).UTC()}
	store := newTestContinuationStore(t, clock, nil)
	input := "patch with \"quotes\" and \\ slash"
	providerWrapper := `{
  "input" : "patch with \"quotes\" and \\ slash"
}`
	turn := PendingTurn{
		Provider: ProviderName,
		Model:    DefaultModel,
		ToolCalls: []UpstreamToolCall{{
			CallID:         "custom-call",
			ProviderCallID: "provider-custom",
			Kind:           bridge.ToolCustom,
			Name:           ApplyPatchUpstreamName,
			Arguments:      providerWrapper,
			Registration: bridge.ToolRegistration{
				Kind:         bridge.ToolCustom,
				InboundName:  ApplyPatchToolName,
				UpstreamName: ApplyPatchUpstreamName,
				WrapperField: ApplyPatchWrapperField,
			},
		}},
	}
	if err := store.Save(turn); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Begin([]bridge.ToolResult{{CallID: "custom-call", Kind: bridge.ToolCustom, Output: "applied"}})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Abort()
	request := bridge.Request{
		Input: []bridge.InputItem{
			bridge.Message{Role: bridge.RoleUser, Content: []bridge.ContentPart{bridge.TextContent{Text: "continue"}}},
			bridge.CustomToolCall{CallID: "custom-call", Name: ApplyPatchToolName, Input: input},
			bridge.CustomToolCallOutput{CallID: "custom-call", Output: "applied"},
		},
		Generation:   bridge.GenerationOptions{Stream: true},
		Continuation: NewContinuationRequest(lease),
	}
	registry, err := NewToolRegistry(request)
	if err != nil {
		t.Fatal(err)
	}
	request.ToolRegistry = registry
	mapped, err := MapRequest(request, DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped.Messages) != 3 {
		t.Fatalf("messages = %#v", mapped.Messages)
	}
	assistant := mapped.Messages[1]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Function.Arguments != providerWrapper {
		t.Fatalf("provider wrapper was not preserved exactly: %#v", assistant.ToolCalls)
	}
}

func TestContinuationStoreCloseIsIdempotentAndRejectsNewState(t *testing.T) {
	clock := &continuationTestClock{now: time.Unix(1700000000, 0).UTC()}
	store := newTestContinuationStore(t, clock, nil)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(testPendingTurn("one")); !errors.Is(err, ErrContinuationClosed) {
		t.Fatalf("save after close = %v", err)
	}
}
