package capture

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var codexVersionPattern = regexp.MustCompile(`(?i)codex(?:[_ -](?:exec|cli))?[/ -]v?([0-9]+\.[0-9]+\.[0-9]+)`) // #nosec G101 -- this is a version parser, not a credential pattern.
var codexVersionValuePattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)

func codexVersionFromUserAgent(userAgent string) string {
	match := codexVersionPattern.FindStringSubmatch(userAgent)
	if len(match) == 2 {
		return match[1]
	}
	return "unknown"
}

func Redact(value any) any {
	return redactValue(value, "", "")
}

func redactValue(value any, key, parent string) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		keys := make([]string, 0, len(typed))
		for name := range typed {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			if isCredentialKey(name) {
				continue
			}
			result[name] = redactValue(typed[name], name, key)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = redactValue(item, key, parent)
		}
		return result
	case string:
		if isSensitiveContainer(key) || isSensitiveContainer(parent) {
			return "<redacted:string>"
		}
		return redactString(typed, key, parent)
	case float64:
		if isSensitiveContainer(key) || isSensitiveContainer(parent) {
			return float64(0)
		}
		if isTimestampKey(key) {
			return float64(0)
		}
		return typed
	case bool:
		if isSensitiveContainer(key) || isSensitiveContainer(parent) {
			return false
		}
		return typed
	default:
		return value
	}
}

func redactString(value, key, parent string) string {
	normalizedKey := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	if isCredentialKey(normalizedKey) {
		return "<redacted:credential>"
	}
	if normalizedKey == "id" || strings.HasSuffix(normalizedKey, "_id") || strings.HasSuffix(normalizedKey, "_uuid") {
		return "<normalized:id>"
	}
	if isTimestampKey(normalizedKey) {
		return "<normalized:timestamp>"
	}
	if isPathKey(normalizedKey) {
		return "<redacted:path>"
	}

	switch normalizedKey {
	case "type", "role", "model", "status", "object", "tool_choice", "format", "effort", "service_tier", "truncation":
		return value
	case "name":
		if isSafeToolName(value) {
			return value
		}
		return "<redacted:tool-name>"
	case "required":
		if isSafeSchemaName(value) {
			return value
		}
	case "include":
		if isSafeIncludeValue(value) {
			return value
		}
	}
	if parent == "include" && isSafeIncludeValue(value) {
		return value
	}
	return "<redacted:string>"
}

func sanitizeHeaders(headers map[string][]string, version string) map[string]string {
	valuesByKey := make(map[string][]string, len(headers))
	keys := make([]string, 0, len(headers))
	for originalKey, values := range headers {
		key := strings.ToLower(originalKey)
		valuesByKey[key] = values
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if isCredentialKey(key) || key == "cookie" || key == "set-cookie" || key == "chatgpt-account-id" {
			continue
		}
		values := valuesByKey[key]
		if len(values) == 0 {
			result[key] = "<empty>"
			continue
		}
		switch key {
		case "user-agent":
			if version == "unknown" {
				result[key] = "<redacted:user-agent>"
			} else {
				result[key] = fmt.Sprintf("codex_exec/%s", version)
			}
		case "host":
			result[key] = "<loopback>"
		case "content-length":
			result[key] = "<normalized:number>"
		case "x-client-request-id", "session-id", "thread-id", "x-codex-window-id":
			result[key] = "<normalized:id>"
		case "x-codex-turn-metadata":
			result[key] = "<redacted:client-metadata>"
		case "accept", "content-type", "originator", "x-codex-beta-features":
			result[key] = strings.Join(values, ",")
		default:
			result[key] = "<redacted:header>"
		}
	}
	return result
}

func isCredentialKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	for _, marker := range []string{"authorization", "api_key", "apikey", "access_token", "auth_token", "secret", "password", "credential", "cookie", "chatgpt_account_id"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return normalized == "token" || strings.HasSuffix(normalized, "_token")
}

func isSensitiveContainer(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	return normalized == "environment" || normalized == "env" || normalized == "environment_values"
}

func isPathKey(key string) bool {
	for _, marker := range []string{"path", "filepath", "file_path", "filename", "cwd", "workdir", "directory", "workspace_root", "absolute_path"} {
		if key == marker || strings.HasSuffix(key, "_"+marker) {
			return true
		}
	}
	return false
}

func isTimestampKey(key string) bool {
	return key == "timestamp" || key == "created_at" || key == "completed_at" || key == "updated_at" || strings.HasSuffix(key, "_at_unix_ms")
}

func isSafeToolName(value string) bool {
	switch value {
	case "exec_command", "write_stdin", "apply_patch", "read_file", "shell", "computer", "web_search", "function":
		return true
	default:
		return false
	}
}

func isSafeSchemaName(value string) bool {
	if value == "" || len(value) > 80 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_.:-", r) {
			continue
		}
		return false
	}
	return true
}

func isSafeIncludeValue(value string) bool {
	return strings.HasPrefix(value, "reasoning.") || strings.HasPrefix(value, "message.") || value == "computer_call_output" || value == "file_search_call.results"
}
