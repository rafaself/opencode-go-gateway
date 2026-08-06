package opencodego

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/rafaself/opencode-go-gateway/internal/bridge"
)

const (
	// ProviderName identifies the adapter that owns the retained state. It is
	// metadata only and is never sent to the Codex-facing Responses stream.
	ProviderName = "opencodego"
	// DefaultContinuationMaxRecordBytes is the safe upper bound for one
	// retained reasoning/tool turn. The server exposes it as a configurable
	// pending-turn limit without exposing the store's private implementation.
	DefaultContinuationMaxRecordBytes = int64(16 << 20)

	defaultContinuationTTL            = 5 * time.Minute
	defaultContinuationConsumingTTL   = 10 * time.Minute
	defaultContinuationGrace          = 30 * time.Second
	defaultContinuationMaxRecords     = 128
	defaultContinuationMaxRecordBytes = DefaultContinuationMaxRecordBytes
	defaultContinuationMaxBytes       = 128 << 20
	defaultContinuationCleanup        = 30 * time.Second
)

// PendingStatus is the lifecycle of one retained upstream assistant turn.
type PendingStatus string

const (
	PendingStatusPending   PendingStatus = "pending"
	PendingStatusConsuming PendingStatus = "consuming"
	PendingStatusConsumed  PendingStatus = "consumed"
	PendingStatusExpired   PendingStatus = "expired"
)

var (
	ErrContinuationClosed       = errors.New("continuation store is closed")
	ErrContinuationUnknown      = errors.New("continuation call ID is unknown")
	ErrContinuationExpired      = errors.New("continuation state has expired")
	ErrContinuationBusy         = errors.New("continuation state is being consumed")
	ErrContinuationConsumed     = errors.New("continuation state was already consumed")
	ErrContinuationKindMismatch = errors.New("continuation result kind does not match the stored tool call")
	ErrContinuationDuplicate    = errors.New("continuation contains a duplicate tool result")
	ErrContinuationMixed        = errors.New("continuation results belong to unrelated turns")
	ErrContinuationIncomplete   = errors.New("continuation does not contain every tool result")
	ErrContinuationCapacity     = errors.New("continuation store capacity is exhausted")
	ErrContinuationInvalid      = errors.New("continuation state is invalid")
	ErrContinuationFinalized    = errors.New("continuation lease is already finalized")
)

// UpstreamToolCall retains the exact provider-side call needed to replay a
// DeepSeek assistant turn. CallID is the immutable Codex correlation ID;
// ProviderCallID is private provider identity and may differ when the
// provider fragmented or delayed an ID. Arguments retain the wrapped
// provider representation for custom tools.
type UpstreamToolCall struct {
	CallID         string
	ProviderCallID string
	Kind           bridge.ToolKind
	Name           string
	Arguments      string
	Registration   bridge.ToolRegistration
}

// PendingTurn is the minimum temporary state required to replay one
// reasoning/tool-call assistant turn. It intentionally does not retain a
// conversation, prompt, or tool output history.
type PendingTurn struct {
	Provider          string
	Model             string
	ReasoningContent  string
	AssistantContent  string
	ToolCalls         []UpstreamToolCall
	ToolRegistrations []bridge.ToolRegistration
	CallIDs           []string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	SizeBytes         int64
	Status            PendingStatus
}

// ContinuationStoreConfig bounds temporary state and controls cleanup. Now is
// injectable solely for deterministic tests; production uses time.Now.
type ContinuationStoreConfig struct {
	TTL time.Duration
	// ConsumingLeaseTTL is a separate finite deadline that starts when
	// Begin reserves a pending turn. It prevents the pending TTL from
	// expiring a valid long-running upstream continuation while still
	// guaranteeing that an abandoned active lease is eventually reclaimed.
	ConsumingLeaseTTL   time.Duration
	ConsumedGracePeriod time.Duration
	MaxRecords          int
	MaxBytesPerRecord   int64
	MaxAggregateBytes   int64
	CleanupInterval     time.Duration
	Now                 func() time.Time
}

func DefaultContinuationStoreConfig() ContinuationStoreConfig {
	return ContinuationStoreConfig{
		TTL:                 defaultContinuationTTL,
		ConsumingLeaseTTL:   defaultContinuationConsumingTTL,
		ConsumedGracePeriod: defaultContinuationGrace,
		MaxRecords:          defaultContinuationMaxRecords,
		MaxBytesPerRecord:   defaultContinuationMaxRecordBytes,
		MaxAggregateBytes:   defaultContinuationMaxBytes,
		CleanupInterval:     defaultContinuationCleanup,
		Now:                 time.Now,
	}
}

func normalizeContinuationStoreConfig(config ContinuationStoreConfig) (ContinuationStoreConfig, error) {
	defaults := DefaultContinuationStoreConfig()
	if config.TTL == 0 {
		config.TTL = defaults.TTL
	}
	if config.ConsumingLeaseTTL == 0 {
		config.ConsumingLeaseTTL = defaults.ConsumingLeaseTTL
	}
	if config.ConsumedGracePeriod == 0 {
		config.ConsumedGracePeriod = defaults.ConsumedGracePeriod
	}
	if config.MaxRecords == 0 {
		config.MaxRecords = defaults.MaxRecords
	}
	if config.MaxBytesPerRecord == 0 {
		config.MaxBytesPerRecord = defaults.MaxBytesPerRecord
	}
	if config.MaxAggregateBytes == 0 {
		config.MaxAggregateBytes = defaults.MaxAggregateBytes
	}
	if config.CleanupInterval == 0 {
		config.CleanupInterval = defaults.CleanupInterval
	}
	if config.Now == nil {
		config.Now = defaults.Now
	}
	if config.TTL <= 0 || config.ConsumingLeaseTTL <= 0 || config.ConsumedGracePeriod <= 0 || config.MaxRecords <= 0 || config.MaxBytesPerRecord <= 0 || config.MaxAggregateBytes <= 0 || config.CleanupInterval <= 0 {
		return ContinuationStoreConfig{}, ErrContinuationInvalid
	}
	return config, nil
}

type continuationRecord struct {
	key            string
	turn           PendingTurn
	bytes          int64
	originalExpiry time.Time
}

// ContinuationStore is an in-memory, mutex-protected store for temporary
// provider continuation state. Save rejects capacity overflow rather than
// evicting a live turn, which makes loss deterministic and preserves retryable
// pending state. Expired call-ID tombstones are separately bounded so they
// cannot turn expiry traffic into unbounded retained state.
type ContinuationStore struct {
	mu             sync.Mutex
	config         ContinuationStoreConfig
	records        map[string]*continuationRecord
	byCallID       map[string]string
	expiredCallIDs map[string]time.Time
	maxExpiredIDs  int
	totalBytes     int64
	closed         bool
	stop           chan struct{}
	done           chan struct{}
	closeOnce      sync.Once
}

func NewContinuationStore(config ContinuationStoreConfig) (*ContinuationStore, error) {
	config, err := normalizeContinuationStoreConfig(config)
	if err != nil {
		return nil, err
	}
	maxExpiredIDs := config.MaxRecords
	maxInt := int(^uint(0) >> 1)
	if maxExpiredIDs <= maxInt/bridge.DefaultMaxFunctionTools {
		maxExpiredIDs *= bridge.DefaultMaxFunctionTools
	} else {
		maxExpiredIDs = maxInt
	}
	store := &ContinuationStore{
		config:         config,
		records:        make(map[string]*continuationRecord),
		byCallID:       make(map[string]string),
		expiredCallIDs: make(map[string]time.Time),
		maxExpiredIDs:  maxExpiredIDs,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
	go store.cleanupLoop()
	return store, nil
}

func (store *ContinuationStore) cleanupLoop() {
	ticker := time.NewTicker(store.config.CleanupInterval)
	defer ticker.Stop()
	defer close(store.done)
	for {
		select {
		case <-ticker.C:
			store.mu.Lock()
			if !store.closed {
				store.cleanupLocked(store.config.Now())
			}
			store.mu.Unlock()
		case <-store.stop:
			return
		}
	}
}

// Close stops periodic cleanup and releases every retained record. It is
// idempotent and waits for the cleanup goroutine, so callers can prove the
// store has no background leak before returning.
func (store *ContinuationStore) Close() error {
	if store == nil {
		return nil
	}
	store.closeOnce.Do(func() {
		store.mu.Lock()
		store.closed = true
		store.records = make(map[string]*continuationRecord)
		store.byCallID = make(map[string]string)
		store.expiredCallIDs = make(map[string]time.Time)
		store.totalBytes = 0
		close(store.stop)
		store.mu.Unlock()
		<-store.done
	})
	return nil
}

// Save stores a finalized assistant tool turn. The store assigns the
// configured TTL and status; caller-supplied expiry/status values cannot
// bypass those policies.
func (store *ContinuationStore) Save(turn PendingTurn) error {
	return store.SaveContext(context.Background(), turn)
}

// SaveContext is Save with a cancellation gate owned by the request that
// produced the turn. The gate is checked before copying, after normalization,
// and immediately before the record becomes visible, so shutdown/client
// cancellation cannot retain a turn after the handler has stopped owning it.
func (store *ContinuationStore) SaveContext(ctx context.Context, turn PendingTurn) error {
	if store == nil {
		return ErrContinuationClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return ErrContinuationClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := store.config.Now().UTC()
	store.cleanupLocked(now)
	if !pendingTurnFitsBounds(turn, store.config.MaxBytesPerRecord, store.config.MaxAggregateBytes-store.totalBytes) {
		return ErrContinuationCapacity
	}
	turn, size, err := normalizePendingTurn(turn, now, store.config.TTL)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if size > store.config.MaxBytesPerRecord {
		return ErrContinuationCapacity
	}
	if len(store.records) >= store.config.MaxRecords || size > store.config.MaxAggregateBytes-store.totalBytes {
		return ErrContinuationCapacity
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	key := turn.ToolCalls[0].CallID
	for _, call := range turn.ToolCalls {
		if _, exists := store.byCallID[call.CallID]; exists {
			return ErrContinuationInvalid
		}
	}
	for _, call := range turn.ToolCalls {
		delete(store.expiredCallIDs, call.CallID)
	}
	record := &continuationRecord{
		key:            key,
		turn:           turn,
		bytes:          size,
		originalExpiry: turn.ExpiresAt,
	}
	store.records[key] = record
	for _, call := range turn.ToolCalls {
		store.byCallID[call.CallID] = key
	}
	store.totalBytes += size
	return nil
}

// pendingTurnFitsBounds is a cheap lower-bound check performed before cloning
// or marshaling caller-owned state. The exact JSON size is still checked by
// normalizePendingTurn, but this pass prevents an oversized pending turn from
// forcing a large temporary copy merely to discover that it cannot be kept.
func pendingTurnFitsBounds(turn PendingTurn, maxRecordBytes, availableAggregateBytes int64) bool {
	if maxRecordBytes <= 0 || availableAggregateBytes <= 0 || len(turn.ToolCalls) == 0 {
		return false
	}
	if int64(len(turn.ToolCalls)) > maxRecordBytes || int64(len(turn.ToolRegistrations)) > maxRecordBytes || int64(len(turn.CallIDs)) > maxRecordBytes {
		return false
	}
	lowerBound := int64(0)
	add := func(size int) bool {
		if size < 0 || int64(size) > maxRecordBytes-lowerBound {
			return false
		}
		lowerBound += int64(size)
		return lowerBound <= availableAggregateBytes
	}
	if !add(len(turn.Provider)) || !add(len(turn.Model)) || !add(len(turn.ReasoningContent)) || !add(len(turn.AssistantContent)) {
		return false
	}
	for _, call := range turn.ToolCalls {
		if !add(len(call.CallID)) || !add(len(call.ProviderCallID)) || !add(len(call.Name)) || !add(len(call.Arguments)) || !add(len(call.Registration.InboundName)) || !add(len(call.Registration.UpstreamName)) || !add(len(call.Registration.WrapperField)) {
			return false
		}
	}
	for _, registration := range turn.ToolRegistrations {
		if !add(len(registration.InboundName)) || !add(len(registration.UpstreamName)) || !add(len(registration.WrapperField)) {
			return false
		}
	}
	for _, callID := range turn.CallIDs {
		if !add(len(callID)) {
			return false
		}
	}
	return true
}

// Lookup returns a safe copy of a pending turn for diagnostics and tests. It
// never exposes result output or writes any state to logs.
func (store *ContinuationStore) Lookup(callID string) (PendingTurn, error) {
	if store == nil {
		return PendingTurn{}, ErrContinuationClosed
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return PendingTurn{}, ErrContinuationClosed
	}
	now := store.config.Now().UTC()
	store.cleanupLocked(now)
	record, err := store.recordForCallLocked(callID, now)
	if err != nil {
		return PendingTurn{}, err
	}
	return clonePendingTurn(record.turn), nil
}

// Begin validates and reserves a complete result set. Only one goroutine can
// own a pending turn at a time; concurrent duplicate submissions receive
// ErrContinuationBusy. The returned lease must be Commit'ed after upstream
// response acceptance or Abort'ed on pre-acceptance failure.
func (store *ContinuationStore) Begin(results []bridge.ToolResult) (*ContinuationLease, error) {
	if store == nil {
		return nil, ErrContinuationClosed
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil, ErrContinuationClosed
	}
	now := store.config.Now().UTC()
	store.cleanupLocked(now)
	if len(results) == 0 {
		return nil, ErrContinuationInvalid
	}
	var first *continuationRecord
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		if result.CallID == "" {
			return nil, ErrContinuationInvalid
		}
		if _, exists := seen[result.CallID]; exists {
			return nil, ErrContinuationDuplicate
		}
		seen[result.CallID] = struct{}{}
		record, err := store.recordForCallLocked(result.CallID, now)
		if err != nil {
			return nil, err
		}
		if first == nil {
			first = record
		} else if first != record {
			return nil, ErrContinuationMixed
		}
	}
	if first.turn.Status == PendingStatusConsuming {
		return nil, ErrContinuationBusy
	}
	if first.turn.Status == PendingStatusConsumed {
		return nil, ErrContinuationConsumed
	}
	if first.turn.Status != PendingStatusPending {
		return nil, ErrContinuationExpired
	}
	if len(results) != len(first.turn.ToolCalls) {
		return nil, ErrContinuationIncomplete
	}
	callKinds := make(map[string]bridge.ToolKind, len(first.turn.ToolCalls))
	for _, call := range first.turn.ToolCalls {
		callKinds[call.CallID] = call.Kind
	}
	for _, result := range results {
		kind, ok := callKinds[result.CallID]
		if !ok {
			return nil, ErrContinuationUnknown
		}
		if result.Kind != kind {
			return nil, ErrContinuationKindMismatch
		}
	}
	first.turn.Status = PendingStatusConsuming
	first.turn.ExpiresAt = now.Add(store.config.ConsumingLeaseTTL)
	return &ContinuationLease{
		store:   store,
		record:  first,
		turn:    clonePendingTurn(first.turn),
		results: cloneToolResults(results),
	}, nil
}

func (store *ContinuationStore) recordForCallLocked(callID string, now time.Time) (*continuationRecord, error) {
	key, ok := store.byCallID[callID]
	if !ok {
		if expiresAt, expired := store.expiredCallIDs[callID]; expired {
			if now.Before(expiresAt) {
				return nil, ErrContinuationExpired
			}
			delete(store.expiredCallIDs, callID)
		}
		return nil, ErrContinuationUnknown
	}
	record, ok := store.records[key]
	if !ok {
		return nil, ErrContinuationUnknown
	}
	if record.turn.Status == PendingStatusPending && !now.Before(record.turn.ExpiresAt) {
		record.turn.Status = PendingStatusExpired
		return nil, ErrContinuationExpired
	}
	if record.turn.Status == PendingStatusConsuming && !now.Before(record.turn.ExpiresAt) {
		record.turn.Status = PendingStatusExpired
		return nil, ErrContinuationExpired
	}
	if record.turn.Status == PendingStatusConsumed && !now.Before(record.turn.ExpiresAt) {
		store.removeLocked(record)
		return nil, ErrContinuationUnknown
	}
	switch record.turn.Status {
	case PendingStatusExpired:
		return nil, ErrContinuationExpired
	case PendingStatusConsumed:
		return record, nil
	case PendingStatusPending, PendingStatusConsuming:
		return record, nil
	default:
		return nil, ErrContinuationInvalid
	}
}

func (store *ContinuationStore) cleanupLocked(now time.Time) {
	for callID, expiresAt := range store.expiredCallIDs {
		if !now.Before(expiresAt) {
			delete(store.expiredCallIDs, callID)
		}
	}
	for _, record := range store.records {
		switch record.turn.Status {
		case PendingStatusPending, PendingStatusConsuming:
			if !now.Before(record.turn.ExpiresAt) {
				store.expireLocked(record, now)
			}
		case PendingStatusConsumed:
			if !now.Before(record.turn.ExpiresAt) {
				store.removeLocked(record)
			}
		case PendingStatusExpired:
			store.expireLocked(record, now)
		}
	}
}

func (store *ContinuationStore) expireLocked(record *continuationRecord, now time.Time) {
	for _, call := range record.turn.ToolCalls {
		if _, exists := store.expiredCallIDs[call.CallID]; exists || len(store.expiredCallIDs) < store.maxExpiredIDs {
			store.expiredCallIDs[call.CallID] = now.Add(store.config.ConsumedGracePeriod)
		}
	}
	record.turn.Status = PendingStatusExpired
	store.removeLocked(record)
}

func (store *ContinuationStore) removeLocked(record *continuationRecord) {
	if _, exists := store.records[record.key]; !exists {
		return
	}
	delete(store.records, record.key)
	for _, call := range record.turn.ToolCalls {
		delete(store.byCallID, call.CallID)
	}
	store.totalBytes -= record.bytes
	if store.totalBytes < 0 {
		store.totalBytes = 0
	}
}

// RecordCount and Bytes expose bounded metrics without exposing retained
// content. They are useful for health checks and deterministic tests.
func (store *ContinuationStore) RecordCount() int {
	if store == nil {
		return 0
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return 0
	}
	store.cleanupLocked(store.config.Now().UTC())
	return len(store.records)
}

func (store *ContinuationStore) Bytes() int64 {
	if store == nil {
		return 0
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return 0
	}
	store.cleanupLocked(store.config.Now().UTC())
	return store.totalBytes
}

// CapacityLimits returns the configured record and aggregate bounds without
// exposing any retained continuation content.
func (store *ContinuationStore) CapacityLimits() (maxRecords int, maxRecordBytes, maxAggregateBytes int64) {
	if store == nil {
		return 0, 0, 0
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return 0, 0, 0
	}
	return store.config.MaxRecords, store.config.MaxBytesPerRecord, store.config.MaxAggregateBytes
}

// ContinuationLease is the only mutable handle returned to an orchestrator.
// Turn and Results are immutable copies. Commit/Abort are idempotent for the
// first terminal action and reject a conflicting second action.
type ContinuationLease struct {
	store   *ContinuationStore
	record  *continuationRecord
	turn    PendingTurn
	results []bridge.ToolResult
	mu      sync.Mutex
	done    bool
}

// ContinuationRequest is opaque bridge state consumed by MapRequest. It
// carries a lease rather than copied reasoning or tool output, so the store
// remains the owner of lifecycle and retry semantics.
type ContinuationRequest struct {
	lease *ContinuationLease
}

func NewContinuationRequest(lease *ContinuationLease) *ContinuationRequest {
	if lease == nil {
		return nil
	}
	return &ContinuationRequest{lease: lease}
}

func (lease *ContinuationLease) Turn() PendingTurn {
	if lease == nil {
		return PendingTurn{}
	}
	return clonePendingTurn(lease.turn)
}

func (lease *ContinuationLease) Results() []bridge.ToolResult {
	if lease == nil {
		return nil
	}
	return cloneToolResults(lease.results)
}

func (lease *ContinuationLease) Commit() error {
	return lease.CommitContext(context.Background())
}

// CommitContext prevents a request that has lost its effective ownership from
// finalizing a continuation after client or shutdown cancellation.
func (lease *ContinuationLease) CommitContext(ctx context.Context) error {
	return lease.finishContext(ctx, true)
}

func (lease *ContinuationLease) Abort() error {
	return lease.finishContext(context.Background(), false)
}

func (lease *ContinuationLease) finishContext(ctx context.Context, commit bool) error {
	if lease == nil || lease.store == nil || lease.record == nil {
		return ErrContinuationInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.done {
		return ErrContinuationFinalized
	}
	store := lease.store
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		lease.done = true
		return ErrContinuationClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if lease.record.turn.Status == PendingStatusExpired {
		lease.done = true
		return ErrContinuationExpired
	}
	if lease.record.turn.Status != PendingStatusConsuming {
		lease.done = true
		return ErrContinuationInvalid
	}
	now := store.config.Now().UTC()
	if !now.Before(lease.record.turn.ExpiresAt) {
		store.expireLocked(lease.record, now)
		lease.done = true
		return ErrContinuationExpired
	}
	if commit {
		lease.record.turn.Status = PendingStatusConsumed
		lease.record.turn.ExpiresAt = now.Add(store.config.ConsumedGracePeriod)
	} else if now.Before(lease.record.originalExpiry) {
		lease.record.turn.Status = PendingStatusPending
		lease.record.turn.ExpiresAt = lease.record.originalExpiry
	} else {
		store.expireLocked(lease.record, now)
	}
	lease.turn = clonePendingTurn(lease.record.turn)
	lease.done = true
	return nil
}

func normalizePendingTurn(turn PendingTurn, now time.Time, ttl time.Duration) (PendingTurn, int64, error) {
	if turn.Provider == "" || turn.Model == "" || len(turn.ToolCalls) == 0 {
		return PendingTurn{}, 0, ErrContinuationInvalid
	}
	turn = clonePendingTurn(turn)
	if turn.CreatedAt.IsZero() {
		turn.CreatedAt = now
	} else {
		turn.CreatedAt = turn.CreatedAt.UTC()
	}
	turn.ExpiresAt = now.Add(ttl)
	turn.Status = PendingStatusPending
	turn.CallIDs = make([]string, 0, len(turn.ToolCalls))
	seenCallIDs := make(map[string]struct{}, len(turn.ToolCalls))
	seenProviderIDs := make(map[string]struct{}, len(turn.ToolCalls))
	for index := range turn.ToolCalls {
		call := &turn.ToolCalls[index]
		if call.CallID == "" {
			return PendingTurn{}, 0, ErrContinuationInvalid
		}
		if call.ProviderCallID == "" {
			call.ProviderCallID = call.CallID
		}
		if call.Name == "" {
			return PendingTurn{}, 0, ErrContinuationInvalid
		}
		if _, exists := seenCallIDs[call.CallID]; exists {
			return PendingTurn{}, 0, ErrContinuationInvalid
		}
		if _, exists := seenProviderIDs[call.ProviderCallID]; exists {
			return PendingTurn{}, 0, ErrContinuationInvalid
		}
		seenCallIDs[call.CallID] = struct{}{}
		seenProviderIDs[call.ProviderCallID] = struct{}{}
		turn.CallIDs = append(turn.CallIDs, call.CallID)
	}
	encoded, err := json.Marshal(turn)
	if err != nil {
		return PendingTurn{}, 0, ErrContinuationInvalid
	}
	turn.SizeBytes = int64(len(encoded))
	return turn, turn.SizeBytes, nil
}

func clonePendingTurn(turn PendingTurn) PendingTurn {
	turn.ToolCalls = append([]UpstreamToolCall(nil), turn.ToolCalls...)
	turn.ToolRegistrations = append([]bridge.ToolRegistration(nil), turn.ToolRegistrations...)
	turn.CallIDs = append([]string(nil), turn.CallIDs...)
	return turn
}

func cloneToolResults(results []bridge.ToolResult) []bridge.ToolResult {
	return append([]bridge.ToolResult(nil), results...)
}
