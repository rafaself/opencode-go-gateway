package opencodego

import "testing"

func TestTaggedModel(t *testing.T) {
	model := TaggedModel(DefaultModel, ProviderTagGo)
	if model != "deepseek-v4-flash (go)" {
		t.Fatalf("TaggedModel() = %q, want %q", model, "deepseek-v4-flash (go)")
	}
	if zen := TaggedModel(DeepSeekV4FlashFreeModel, ProviderTagZen); zen != "deepseek-v4-flash-free (zen)" {
		t.Fatalf("TaggedModel() = %q, want %q", zen, "deepseek-v4-flash-free (zen)")
	}
}

func TestSplitTaggedModel(t *testing.T) {
	tests := []struct {
		name  string
		model string
		label string
		tag   ProviderTag
		ok    bool
	}{
		{name: "go backend", model: "deepseek-v4-flash (go)", label: "deepseek-v4-flash", tag: ProviderTagGo, ok: true},
		{name: "zen backend", model: "deepseek-v4-flash (zen)", label: "deepseek-v4-flash", tag: ProviderTagZen, ok: true},
		{name: "labeled go", model: "deepseek-v4-flash-free (zen)", label: "deepseek-v4-flash-free", tag: ProviderTagZen, ok: true},
		{name: "untagged", model: "deepseek-v4-flash"},
		{name: "empty", model: ""},
		{name: "tag only", model: "(go)"},
		{name: "unknown tag", model: "deepseek-v4-flash (pro)"},
		{name: "trailing whitespace", model: "deepseek-v4-flash (go) "},
		{name: "missing close paren", model: "deepseek-v4-flash (go"},
		{name: "nested suffix", model: "deepseek (go) (zen)"},
		{name: "space in label", model: "deep seek (go)"},
		{name: "slash in label", model: "deepseek/v1 (go)"},
		{name: "empty label with space", model: " (go)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			label, tag, ok := SplitTaggedModel(test.model)
			if ok != test.ok {
				t.Fatalf("SplitTaggedModel(%q) ok = %v, want %v", test.model, ok, test.ok)
			}
			if !ok {
				return
			}
			if label != test.label || tag != test.tag {
				t.Fatalf("SplitTaggedModel(%q) = (%q, %q), want (%q, %q)", test.model, label, tag, test.label, test.tag)
			}
		})
	}
}
