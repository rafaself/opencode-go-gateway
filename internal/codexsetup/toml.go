package codexsetup

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This is a deliberately conservative TOML validator/editor. It validates
// the TOML constructs that Codex configuration uses, preserves untouched
// source lines and comments, and refuses ambiguous constructs rather than
// rewriting them with a regex. It is not a general-purpose TOML library.
type tomlDocument struct {
	lines []string
	keys  map[string]tomlAssignment
}

type tomlAssignment struct {
	line    int
	endLine int
	table   string
	key     string
	value   string
}

func parseTOML(source string) (tomlDocument, error) {
	if !utf8.ValidString(source) {
		return tomlDocument{}, fmt.Errorf("TOML is not valid UTF-8")
	}
	lines := strings.SplitAfter(source, "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	document := tomlDocument{lines: lines, keys: make(map[string]tomlAssignment)}
	currentTable := ""
	tables := make(map[string]int)
	arrayTables := make(map[string]int)
	for lineNumber := 0; lineNumber < len(lines); lineNumber++ {
		raw := lines[lineNumber]
		line := strings.TrimSuffix(raw, "\n")
		line = strings.TrimSuffix(line, "\r")
		withoutComment, err := tomlCommentFree(line)
		if err != nil {
			return tomlDocument{}, fmt.Errorf("line %d: %w", lineNumber+1, err)
		}
		trimmed := strings.TrimSpace(withoutComment)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			table, array, err := parseTableHeader(trimmed)
			if err != nil {
				return tomlDocument{}, fmt.Errorf("line %d: %w", lineNumber+1, err)
			}
			if array {
				arrayTables[table]++
				// Array-of-table instances can intentionally repeat keys. Give
				// each instance an internal identity while preserving its source
				// lines unchanged.
				currentTable = fmt.Sprintf("%s#array%d", table, arrayTables[table])
				continue
			}
			if _, exists := tables[table]; exists {
				return tomlDocument{}, fmt.Errorf("line %d: duplicate table %q", lineNumber+1, table)
			}
			tables[table] = lineNumber
			currentTable = table
			continue
		}
		equals := tomlEqualsIndex(withoutComment)
		if equals < 0 {
			return tomlDocument{}, fmt.Errorf("line %d: expected a table header or key/value assignment", lineNumber+1)
		}
		keyText := strings.TrimSpace(withoutComment[:equals])
		key, err := parseDottedKey(keyText)
		if err != nil {
			return tomlDocument{}, fmt.Errorf("line %d: %w", lineNumber+1, err)
		}
		value := strings.TrimSpace(withoutComment[equals+1:])
		endLine := lineNumber
		if isCollectionValue(value) {
			var err error
			value, endLine, err = collectTOMLCollection(lines, lineNumber, value)
			if err != nil {
				return tomlDocument{}, fmt.Errorf("line %d key %q: %w", lineNumber+1, key, err)
			}
		}
		if err := validateTOMLValue(value); err != nil {
			return tomlDocument{}, fmt.Errorf("line %d key %q: %w", lineNumber+1, key, err)
		}
		fullKey := key
		if currentTable != "" {
			fullKey = currentTable + "." + key
		}
		if previous, exists := document.keys[fullKey]; exists {
			return tomlDocument{}, fmt.Errorf("line %d: duplicate key %q (line %d)", lineNumber+1, fullKey, previous.line+1)
		}
		document.keys[fullKey] = tomlAssignment{line: lineNumber, endLine: endLine, table: currentTable, key: key, value: value}
		lineNumber = endLine
	}
	return document, nil
}

func (document tomlDocument) value(table, key string) (string, bool) {
	assignment, ok := document.keys[key]
	if table != "" {
		assignment, ok = document.keys[table+"."+key]
	}
	if !ok {
		return "", false
	}
	return assignment.value, true
}

func parseTableHeader(value string) (string, bool, error) {
	array := strings.HasPrefix(value, "[[")
	open, close := "[", "]"
	if array {
		open, close = "[[", "]]"
	}
	if !strings.HasPrefix(value, open) || !strings.HasSuffix(value, close) || len(value) <= len(open)+len(close) {
		return "", array, fmt.Errorf("invalid table header")
	}
	inside := strings.TrimSpace(value[len(open) : len(value)-len(close)])
	if strings.Contains(value[len(open):len(value)-len(close)], close) {
		return "", array, fmt.Errorf("invalid table header")
	}
	key, err := parseDottedKey(inside)
	if err != nil {
		return "", array, fmt.Errorf("invalid table header: %w", err)
	}
	return key, array, nil
}

func parseDottedKey(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("empty key")
	}
	parts := splitOutsideQuotes(value, '.')
	for index := range parts {
		part := strings.TrimSpace(parts[index])
		if part == "" {
			return "", fmt.Errorf("empty dotted key")
		}
		if (strings.HasPrefix(part, `"`) && strings.HasSuffix(part, `"`)) || (strings.HasPrefix(part, "'") && strings.HasSuffix(part, "'")) {
			decoded, err := parseTOMLString(part)
			if err != nil {
				return "", err
			}
			parts[index] = decoded
			continue
		}
		for _, runeValue := range part {
			if (runeValue >= 'a' && runeValue <= 'z') || (runeValue >= 'A' && runeValue <= 'Z') || (runeValue >= '0' && runeValue <= '9') || runeValue == '_' || runeValue == '-' {
				continue
			}
			return "", fmt.Errorf("invalid key %q", part)
		}
		parts[index] = part
	}
	return strings.Join(parts, "."), nil
}

func tomlCommentFree(value string) (string, error) {
	var quote rune
	escaped := false
	for index, runeValue := range value {
		if quote == '"' {
			if escaped {
				escaped = false
				continue
			}
			if runeValue == '\\' {
				escaped = true
				continue
			}
			if runeValue == quote {
				quote = 0
			}
			continue
		}
		if quote == '\'' {
			if runeValue == quote {
				quote = 0
			}
			continue
		}
		if runeValue == '"' || runeValue == '\'' {
			quote = runeValue
			continue
		}
		if runeValue == '#' {
			return value[:index], nil
		}
	}
	if quote != 0 || escaped {
		return "", fmt.Errorf("unterminated string")
	}
	return value, nil
}

func tomlEqualsIndex(value string) int {
	var quote rune
	escaped := false
	depth := 0
	for index, runeValue := range value {
		if quote == '"' {
			if escaped {
				escaped = false
				continue
			}
			if runeValue == '\\' {
				escaped = true
				continue
			}
			if runeValue == quote {
				quote = 0
			}
			continue
		}
		if quote == '\'' {
			if runeValue == quote {
				quote = 0
			}
			continue
		}
		switch runeValue {
		case '"', '\'':
			quote = runeValue
		case '[', '{':
			depth++
		case ']', '}':
			if depth > 0 {
				depth--
			}
		case '=':
			if depth == 0 {
				return index
			}
		}
	}
	return -1
}

func validateTOMLValue(value string) error {
	if value == "" {
		return fmt.Errorf("empty value")
	}
	if (strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`)) || (strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'")) {
		_, err := parseTOMLString(value)
		return err
	}
	if value == "true" || value == "false" {
		return nil
	}
	if value[0] == '[' || value[0] == '{' {
		return validateBalancedValue(value)
	}
	for _, runeValue := range value {
		if (runeValue >= 'a' && runeValue <= 'z') || (runeValue >= 'A' && runeValue <= 'Z') || (runeValue >= '0' && runeValue <= '9') || strings.ContainsRune("._+-:TZ ", runeValue) {
			continue
		}
		return fmt.Errorf("unsupported value syntax")
	}
	return nil
}

func isCollectionValue(value string) bool {
	return value != "" && (value[0] == '[' || value[0] == '{')
}

func collectTOMLCollection(lines []string, startLine int, value string) (string, int, error) {
	complete, err := collectionValueComplete(value)
	if err != nil {
		return "", startLine, err
	}
	if complete {
		return value, startLine, nil
	}

	collected := value
	for lineNumber := startLine + 1; lineNumber < len(lines); lineNumber++ {
		line := strings.TrimSuffix(lines[lineNumber], "\n")
		line = strings.TrimSuffix(line, "\r")
		withoutComment, err := tomlCommentFree(line)
		if err != nil {
			return "", lineNumber, err
		}
		collected += "\n" + strings.TrimSpace(withoutComment)
		complete, err = collectionValueComplete(collected)
		if err != nil {
			return "", lineNumber, err
		}
		if complete {
			return collected, lineNumber, nil
		}
	}
	return "", startLine, fmt.Errorf("unbalanced collection value")
}

type collectionScan struct {
	stack   []rune
	quote   rune
	escaped bool
}

func scanCollection(value string) (collectionScan, error) {
	scan := collectionScan{stack: make([]rune, 0, 4)}
	for _, runeValue := range value {
		if scan.quote == '"' {
			if scan.escaped {
				scan.escaped = false
				continue
			}
			if runeValue == '\\' {
				scan.escaped = true
				continue
			}
			if runeValue == scan.quote {
				scan.quote = 0
			}
			continue
		}
		if scan.quote == '\'' {
			if runeValue == scan.quote {
				scan.quote = 0
			}
			continue
		}
		switch runeValue {
		case '"', '\'':
			scan.quote = runeValue
		case '[', '{':
			scan.stack = append(scan.stack, runeValue)
		case ']', '}':
			if len(scan.stack) == 0 || (runeValue == ']' && scan.stack[len(scan.stack)-1] != '[') || (runeValue == '}' && scan.stack[len(scan.stack)-1] != '{') {
				return collectionScan{}, fmt.Errorf("unbalanced collection value")
			}
			scan.stack = scan.stack[:len(scan.stack)-1]
		}
	}
	return scan, nil
}

func collectionValueComplete(value string) (bool, error) {
	scan, err := scanCollection(value)
	if err != nil {
		return false, err
	}
	if scan.quote != 0 || scan.escaped {
		return false, fmt.Errorf("unbalanced collection value")
	}
	if len(scan.stack) != 0 {
		return false, nil
	}
	if err := validateBalancedValue(value); err != nil {
		return false, err
	}
	return true, nil
}

func validateBalancedValue(value string) error {
	scan, err := scanCollection(value)
	if err != nil {
		return err
	}
	if scan.quote != 0 || scan.escaped || len(scan.stack) != 0 {
		return fmt.Errorf("unbalanced collection value")
	}
	if (value[0] == '[' && value[len(value)-1] != ']') || (value[0] == '{' && value[len(value)-1] != '}') {
		return fmt.Errorf("collection value has trailing content")
	}
	return nil
}

func parseTOMLString(value string) (string, error) {
	if strings.HasPrefix(value, `"`) {
		if len(value) < 2 || !strings.HasSuffix(value, `"`) {
			return "", fmt.Errorf("unterminated basic string")
		}
		decoded, err := strconv.Unquote(value)
		if err != nil {
			return "", fmt.Errorf("invalid basic string")
		}
		return decoded, nil
	}
	if strings.HasPrefix(value, "'") {
		if len(value) < 2 || !strings.HasSuffix(value, "'") || strings.Contains(value[1:len(value)-1], "'") {
			return "", fmt.Errorf("invalid literal string")
		}
		return value[1 : len(value)-1], nil
	}
	return "", fmt.Errorf("expected TOML string")
}

func splitOutsideQuotes(value string, separator rune) []string {
	var quote rune
	escaped := false
	start := 0
	parts := make([]string, 0, 2)
	for index, runeValue := range value {
		if quote == '"' {
			if escaped {
				escaped = false
				continue
			}
			if runeValue == '\\' {
				escaped = true
				continue
			}
			if runeValue == quote {
				quote = 0
			}
			continue
		}
		if quote == '\'' {
			if runeValue == quote {
				quote = 0
			}
			continue
		}
		if runeValue == '"' || runeValue == '\'' {
			quote = runeValue
			continue
		}
		if runeValue == separator {
			parts = append(parts, value[start:index])
			start = index + len(string(runeValue))
		}
	}
	parts = append(parts, value[start:])
	return parts
}
