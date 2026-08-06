package bridge

// Request and stream safety limits are shared by the Codex decoder, provider
// mapper, and stream adapters. They bound request-controlled allocations while
// leaving JSON Schema and model argument semantics to their owning systems.
const (
	DefaultMaxInputItems      = 256
	DefaultMaxCollectionItems = 256
	DefaultMaxJSONDepth       = 64
	DefaultMaxJSONTokens      = 1 << 20
	// DefaultMaxProviderTools is the total provider-visible function budget.
	// Synthetic adapter functions, including apply_patch, consume a slot.
	DefaultMaxProviderTools         = 128
	DefaultMaxFunctionTools         = DefaultMaxProviderTools
	DefaultMaxFunctionSchemaBytes   = 256 << 10
	DefaultMaxToolCallArgumentBytes = 1 << 20
	DefaultMaxStreamToolCalls       = 128
	DefaultMaxStreamChoices         = 16
	DefaultMaxToolNameBytes         = 64
	DefaultMaxProviderCallIDBytes   = 256
	DefaultMaxOutputBytes           = 16 << 20
	DefaultMaxTextBytes             = 8 << 20
	DefaultMaxReasoningBytes        = 8 << 20
	DefaultMaxActiveRequests        = 64
)
