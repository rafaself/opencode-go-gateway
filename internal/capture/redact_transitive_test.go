package capture

import "testing"

func TestRedactPropagatesSensitiveContextTransitively(t *testing.T) {
	t.Parallel()

	for name, subtree := range map[string]any{
		"environment":        sensitiveRedactionPayload(),
		"env":                []any{sensitiveRedactionPayload()},
		"environment_values": []any{sensitiveRedactionPayload()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			input := map[string]any{
				"model": name + "-outside-model",
				name:    subtree,
			}

			redactedValue := Redact(input)
			redacted, ok := redactedValue.(map[string]any)
			if !ok {
				t.Fatalf("redacted value has type %T, want map[string]any", redactedValue)
			}
			if got, want := redacted["model"], name+"-outside-model"; got != want {
				t.Fatalf("non-sensitive model = %#v, want %#v", got, want)
			}

			assertSensitiveRedactionPayload(t, redacted[name])
		})
	}
}

func sensitiveRedactionPayload() map[string]any {
	return map[string]any{
		"nested": map[string]any{
			"model":   "nested-sensitive-model",
			"number":  float64(42),
			"enabled": true,
			"items": []any{
				map[string]any{
					"model":   "array-sensitive-model",
					"number":  float64(7),
					"enabled": true,
				},
				"array-sensitive-string",
			},
		},
	}
}

func assertSensitiveRedactionPayload(t *testing.T, value any) {
	t.Helper()

	if values, ok := value.([]any); ok {
		if len(values) != 1 {
			t.Fatalf("sensitive array length = %d, want 1", len(values))
		}
		value = values[0]
	}

	root, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("sensitive subtree has type %T, want map[string]any", value)
	}
	nested, ok := root["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested sensitive value has type %T, want map[string]any", root["nested"])
	}

	assertRedactedString(t, nested["model"])
	assertRedactedNumber(t, nested["number"])
	assertRedactedBoolean(t, nested["enabled"])

	items, ok := nested["items"].([]any)
	if !ok {
		t.Fatalf("sensitive items have type %T, want []any", nested["items"])
	}
	if len(items) != 2 {
		t.Fatalf("sensitive items length = %d, want 2", len(items))
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("sensitive array item has type %T, want map[string]any", items[0])
	}
	assertRedactedString(t, item["model"])
	assertRedactedNumber(t, item["number"])
	assertRedactedBoolean(t, item["enabled"])
	assertRedactedString(t, items[1])
}

func assertRedactedString(t *testing.T, value any) {
	t.Helper()
	if got, want := value, "<redacted:string>"; got != want {
		t.Fatalf("sensitive string = %#v, want %#v", got, want)
	}
}

func assertRedactedNumber(t *testing.T, value any) {
	t.Helper()
	if got, want := value, float64(0); got != want {
		t.Fatalf("sensitive number = %#v, want %#v", got, want)
	}
}

func assertRedactedBoolean(t *testing.T, value any) {
	t.Helper()
	if got, want := value, false; got != want {
		t.Fatalf("sensitive boolean = %#v, want %#v", got, want)
	}
}
