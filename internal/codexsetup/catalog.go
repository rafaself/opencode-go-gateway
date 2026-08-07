package codexsetup

import (
	"encoding/json"
	"fmt"
)

// Catalog is the subset of the Codex model catalog schema needed by this
// gateway. The fields intentionally mirror the official DeepSeek integration
// catalog rather than inventing a provider-specific schema.
type Catalog struct {
	Models []CatalogModel `json:"models"`
}

type CatalogModel struct {
	Slug                           string           `json:"slug"`
	PreferWebsockets               bool             `json:"prefer_websockets"`
	SupportVerbosity               bool             `json:"support_verbosity"`
	DefaultVerbosity               string           `json:"default_verbosity"`
	ApplyPatchToolType             string           `json:"apply_patch_tool_type"`
	WebSearchToolType              string           `json:"web_search_tool_type"`
	InputModalities                []string         `json:"input_modalities"`
	SupportsImageDetailOriginal    bool             `json:"supports_image_detail_original"`
	TruncationPolicy               TruncationPolicy `json:"truncation_policy"`
	SupportsParallelToolCalls      bool             `json:"supports_parallel_tool_calls"`
	ToolMode                       *string          `json:"tool_mode"`
	MultiAgentVersion              string           `json:"multi_agent_version"`
	UseResponsesLite               bool             `json:"use_responses_lite"`
	IncludeSkillsUsageInstructions bool             `json:"include_skills_usage_instructions"`
	AutoReviewModelOverride        *string          `json:"auto_review_model_override"`
	ContextWindow                  int64            `json:"context_window"`
	MaxContextWindow               int64            `json:"max_context_window"`
	EffectiveContextWindowPercent  int              `json:"effective_context_window_percent"`
	AutoCompactTokenLimit          *int64           `json:"auto_compact_token_limit"`
	CompHash                       string           `json:"comp_hash"`
	ReasoningSummaryFormat         string           `json:"reasoning_summary_format"`
	DefaultReasoningSummary        string           `json:"default_reasoning_summary"`
	DisplayName                    string           `json:"display_name"`
	Description                    string           `json:"description"`
	DefaultReasoningLevel          string           `json:"default_reasoning_level"`
	SupportedReasoningLevels       []ReasoningLevel `json:"supported_reasoning_levels"`
	ShellType                      string           `json:"shell_type"`
	Visibility                     string           `json:"visibility"`
	MinimalClientVersion           string           `json:"minimal_client_version"`
	SupportedInAPI                 bool             `json:"supported_in_api"`
	AvailabilityNUX                *string          `json:"availability_nux"`
	Upgrade                        *string          `json:"upgrade"`
	Priority                       int              `json:"priority"`
	BaseInstructions               string           `json:"base_instructions"`
	ExperimentalSupportedTools     []string         `json:"experimental_supported_tools"`
}

type TruncationPolicy struct {
	Mode  string `json:"mode"`
	Limit int64  `json:"limit"`
}

type ReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

func generatedCatalog() Catalog {
	return Catalog{Models: []CatalogModel{
		{
			Slug:                           GoModelID,
			PreferWebsockets:               false,
			SupportVerbosity:               true,
			DefaultVerbosity:               "low",
			ApplyPatchToolType:             "freeform",
			WebSearchToolType:              "text",
			InputModalities:                []string{"text"},
			SupportsImageDetailOriginal:    false,
			TruncationPolicy:               TruncationPolicy{Mode: "tokens", Limit: 10000},
			SupportsParallelToolCalls:      true,
			ToolMode:                       nil,
			MultiAgentVersion:              "v2",
			UseResponsesLite:               false,
			IncludeSkillsUsageInstructions: false,
			AutoReviewModelOverride:        nil,
			ContextWindow:                  1048576,
			MaxContextWindow:               1048576,
			EffectiveContextWindowPercent:  95,
			AutoCompactTokenLimit:          nil,
			CompHash:                       "3000",
			ReasoningSummaryFormat:         "experimental",
			DefaultReasoningSummary:        "none",
			DisplayName:                    "DeepSeek-V4-Flash",
			Description:                    "Latest frontier agentic coding model.",
			DefaultReasoningLevel:          "high",
			SupportedReasoningLevels: []ReasoningLevel{
				{Effort: "low", Description: "Fast responses with lighter reasoning"},
				{Effort: "high", Description: "Extra high reasoning depth for complex problems"},
				{Effort: "max", Description: "Maximum reasoning depth for the hardest problems"},
			},
			ShellType:                  "shell_command",
			Visibility:                 "list",
			MinimalClientVersion:       "0.144.0",
			SupportedInAPI:             true,
			AvailabilityNUX:            nil,
			Upgrade:                    nil,
			Priority:                   1,
			BaseInstructions:           baseInstructions,
			ExperimentalSupportedTools: []string{},
		},
		{
			Slug:                           ZenFreeModelID,
			PreferWebsockets:               false,
			SupportVerbosity:               true,
			DefaultVerbosity:               "low",
			ApplyPatchToolType:             "freeform",
			WebSearchToolType:              "text",
			InputModalities:                []string{"text"},
			SupportsImageDetailOriginal:    false,
			TruncationPolicy:               TruncationPolicy{Mode: "tokens", Limit: 10000},
			SupportsParallelToolCalls:      true,
			ToolMode:                       nil,
			MultiAgentVersion:              "v2",
			UseResponsesLite:               false,
			IncludeSkillsUsageInstructions: false,
			AutoReviewModelOverride:        nil,
			// The Zen free alias serves the same DeepSeek model with a reduced
			// free-tier context window; Codex compacts conservatively when the
			// declared window is smaller than the upstream capability.
			ContextWindow:                 200000,
			MaxContextWindow:              200000,
			EffectiveContextWindowPercent: 95,
			AutoCompactTokenLimit:         nil,
			CompHash:                      "3000",
			ReasoningSummaryFormat:        "experimental",
			DefaultReasoningSummary:       "none",
			DisplayName:                   "DeepSeek-V4-Flash-Free",
			Description:                   "Free DeepSeek V4 Flash served by OpenCode Zen while the free tier is available.",
			DefaultReasoningLevel:         "high",
			SupportedReasoningLevels: []ReasoningLevel{
				{Effort: "low", Description: "Fast responses with lighter reasoning"},
				{Effort: "high", Description: "Extra high reasoning depth for complex problems"},
				{Effort: "max", Description: "Maximum reasoning depth for the hardest problems"},
			},
			ShellType:                  "shell_command",
			Visibility:                 "list",
			MinimalClientVersion:       "0.144.0",
			SupportedInAPI:             true,
			AvailabilityNUX:            nil,
			Upgrade:                    nil,
			Priority:                   2,
			BaseInstructions:           baseInstructions,
			ExperimentalSupportedTools: []string{},
		},
	}}
}

// baseInstructions is the fixed model-role instruction Codex requires in the
// catalog. It is deliberately short, generic, and does not claim an OpenAI
// identity or embed any user, project, or credential content.
const baseInstructions = "You are an expert coding agent. Work with the user in their workspace to complete their requested task, using the provided tools when they help."

// GenerateCatalog returns stable, indented JSON suitable for an atomic file
// write. It never reads credentials or provider responses.
func GenerateCatalog() ([]byte, error) {
	value, err := json.MarshalIndent(generatedCatalog(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode model catalog: %w", err)
	}
	return append(value, '\n'), nil
}

// ValidateCatalog verifies JSON syntax and the fields Codex needs for this
// provider. It requires both managed models with the full supported metadata.
// Unknown fields remain accepted so a future Codex catalog can be inspected
// without being rejected by this gateway.
func ValidateCatalog(data []byte) (Catalog, error) {
	var catalog Catalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return Catalog{}, fmt.Errorf("decode model catalog: %w", err)
	}
	if len(catalog.Models) == 0 {
		return Catalog{}, fmt.Errorf("model catalog has no models")
	}
	seen := make(map[string]bool, len(catalog.Models))
	for _, model := range catalog.Models {
		if model.Slug == "" || model.DisplayName == "" {
			return Catalog{}, fmt.Errorf("model catalog contains a model without slug or display name")
		}
		if model.BaseInstructions == "" {
			return Catalog{}, fmt.Errorf("model catalog contains a model without base instructions")
		}
		if model.Slug != GoModelID && model.Slug != ZenFreeModelID {
			continue
		}
		if model.ContextWindow <= 0 || model.MaxContextWindow < model.ContextWindow {
			return Catalog{}, fmt.Errorf("model %q has invalid context window", model.Slug)
		}
		if model.EffectiveContextWindowPercent <= 0 || model.EffectiveContextWindowPercent > 100 || model.TruncationPolicy.Mode != "tokens" || model.TruncationPolicy.Limit <= 0 {
			return Catalog{}, fmt.Errorf("model %q has invalid compaction metadata", model.Slug)
		}
		if len(model.InputModalities) != 1 || model.InputModalities[0] != "text" {
			return Catalog{}, fmt.Errorf("model %q must declare text-only input", model.Slug)
		}
		if model.PreferWebsockets || model.ApplyPatchToolType != "freeform" || model.WebSearchToolType != "text" || !model.SupportsParallelToolCalls {
			return Catalog{}, fmt.Errorf("model %q has incompatible tool or transport capabilities", model.Slug)
		}
		if !model.SupportedInAPI || model.DefaultReasoningSummary != "none" || model.ReasoningSummaryFormat == "" {
			return Catalog{}, fmt.Errorf("model %q has incompatible reasoning or availability metadata", model.Slug)
		}
		if model.DefaultReasoningLevel == "" || len(model.SupportedReasoningLevels) == 0 {
			return Catalog{}, fmt.Errorf("model %q has no reasoning levels", model.Slug)
		}
		foundDefault := false
		for _, effort := range model.SupportedReasoningLevels {
			if effort.Effort == model.DefaultReasoningLevel {
				foundDefault = true
			}
		}
		if !foundDefault {
			return Catalog{}, fmt.Errorf("model %q default reasoning level is unsupported", model.Slug)
		}
		seen[model.Slug] = true
	}
	for _, slug := range []string{GoModelID, ZenFreeModelID} {
		if !seen[slug] {
			return Catalog{}, fmt.Errorf("model catalog does not include %q", slug)
		}
	}
	return catalog, nil
}
