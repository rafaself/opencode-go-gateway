package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
)

// validateJSONSchema checks the structural JSON Schema boundary without
// attempting to interpret provider-independent validation keywords. Unknown
// keywords are intentionally retained in the raw payload and are allowed by
// JSON Schema's extension model.
//
// The returned errors deliberately contain no schema paths or values. Schema
// keys and values are request-controlled, and this function is called while
// constructing client-facing boundary errors.
func validateJSONSchema(raw json.RawMessage, limits DecoderLimits) error {
	if int64(len(raw)) > limits.MaxSchemaBytes {
		return errors.New("schema exceeds the configured byte limit")
	}
	if err := validateJSONDocument(raw, limits.MaxJSONDepth, limits.MaxJSONTokens, limits.MaxCollectionItems); err != nil {
		return errors.New("schema structure exceeds the configured limit")
	}
	fields, err := schemaObject(raw)
	if err != nil {
		return err
	}
	return validateSchemaObject(fields, limits.MaxCollectionItems)
}

func schemaObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("schema root must be an object")
	}
	return fields, nil
}

func validateSchemaObject(fields map[string]json.RawMessage, maxCollectionItems int) error {
	if raw, ok := fields["type"]; ok {
		if err := validateSchemaType(raw, maxCollectionItems); err != nil {
			return err
		}
	}
	for _, keyword := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
		raw, ok := fields[keyword]
		if !ok {
			continue
		}
		properties, err := schemaObject(raw)
		if err != nil {
			return errors.New("schema properties must be an object")
		}
		for _, name := range sortedSchemaKeys(properties) {
			if err := validateSchemaValue(properties[name], maxCollectionItems); err != nil {
				return err
			}
		}
	}
	if raw, ok := fields["required"]; ok {
		if err := validateStringArray(raw, false, maxCollectionItems); err != nil {
			return err
		}
	}
	for _, keyword := range []string{"additionalProperties", "additionalItems", "unevaluatedProperties", "unevaluatedItems", "contains", "propertyNames", "not", "if", "then", "else", "contentSchema"} {
		if raw, ok := fields[keyword]; ok {
			if err := validateSchemaValue(raw, maxCollectionItems); err != nil {
				return err
			}
		}
	}
	for _, keyword := range []string{"items", "prefixItems", "allOf", "anyOf", "oneOf"} {
		if raw, ok := fields[keyword]; ok {
			if err := validateSchemaOrArray(raw, maxCollectionItems); err != nil {
				return err
			}
		}
	}
	if raw, ok := fields["dependencies"]; ok {
		dependencies, err := schemaObject(raw)
		if err != nil {
			return errors.New("schema dependencies must be an object")
		}
		for _, name := range sortedSchemaKeys(dependencies) {
			value := bytes.TrimSpace(dependencies[name])
			if len(value) > 0 && value[0] == '[' {
				if err := validateStringArray(value, false, maxCollectionItems); err != nil {
					return err
				}
				continue
			}
			if err := validateSchemaValue(value, maxCollectionItems); err != nil {
				return err
			}
		}
	}
	if raw, ok := fields["dependentRequired"]; ok {
		dependentRequired, err := schemaObject(raw)
		if err != nil {
			return errors.New("schema dependentRequired must be an object")
		}
		for _, name := range sortedSchemaKeys(dependentRequired) {
			if err := validateStringArray(dependentRequired[name], false, maxCollectionItems); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSchemaValue(raw json.RawMessage, maxCollectionItems int) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return errors.New("schema value is empty")
	}
	if bytes.Equal(trimmed, []byte("true")) || bytes.Equal(trimmed, []byte("false")) {
		return nil
	}
	fields, err := schemaObject(trimmed)
	if err != nil {
		return errors.New("schema value must be an object or boolean")
	}
	return validateSchemaObject(fields, maxCollectionItems)
}

func validateSchemaOrArray(raw json.RawMessage, maxCollectionItems int) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		values, err := rawArray(trimmed, "schema", maxCollectionItems)
		if err != nil {
			return errors.New("schema value must be an object, boolean, or array")
		}
		for _, value := range values {
			if err := validateSchemaValue(value, maxCollectionItems); err != nil {
				return err
			}
		}
		return nil
	}
	return validateSchemaValue(trimmed, maxCollectionItems)
}

func validateSchemaType(raw json.RawMessage, maxCollectionItems int) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return errors.New("schema type must be a string or array of strings")
		}
		if !validSchemaTypeName(value) {
			return errors.New("schema type is not supported")
		}
		return nil
	}
	values, err := rawArray(raw, "schema.type", maxCollectionItems)
	if err != nil || len(values) == 0 {
		return errors.New("schema type must be a string or non-empty array of strings")
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		var name string
		if err := json.Unmarshal(value, &name); err != nil || !validSchemaTypeName(name) {
			return errors.New("schema type array contains an unsupported value")
		}
		if _, exists := seen[name]; exists {
			return errors.New("schema type array contains duplicate values")
		}
		seen[name] = struct{}{}
	}
	return nil
}

func validSchemaTypeName(value string) bool {
	switch value {
	case "null", "boolean", "object", "array", "number", "integer", "string":
		return true
	default:
		return false
	}
}

func validateStringArray(raw json.RawMessage, requireNonEmpty bool, maxCollectionItems int) error {
	values, err := rawArray(raw, "schema", maxCollectionItems)
	if err != nil {
		return errors.New("schema value must be an array of strings")
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		var name string
		if err := json.Unmarshal(value, &name); err != nil || (requireNonEmpty && name == "") {
			return errors.New("schema array contains a non-string value")
		}
		if _, exists := seen[name]; exists {
			return errors.New("schema array contains duplicate values")
		}
		seen[name] = struct{}{}
	}
	return nil
}

func sortedSchemaKeys(fields map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
