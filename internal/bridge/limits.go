package bridge

// Request and stream safety limits are shared by the Codex decoder, provider
// mapper, and stream adapters. They bound request-controlled allocations while
// leaving JSON Schema and model argument semantics to their owning systems.
const (
	DefaultMaxFunctionTools         = 128
	DefaultMaxFunctionSchemaBytes   = 256 << 10
	DefaultMaxToolCallArgumentBytes = 1 << 20
)
