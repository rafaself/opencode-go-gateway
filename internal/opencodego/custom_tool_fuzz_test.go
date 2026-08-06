package opencodego

import "testing"

func FuzzCustomToolArguments(f *testing.F) {
	f.Add([]byte(`{"input":"*** Begin Patch\n*** End Patch"}`))
	f.Add([]byte(`{"input":"Olá, 世界"}`))
	f.Add([]byte(`{"input":null}`))
	f.Fuzz(func(t *testing.T, arguments []byte) {
		if len(arguments) > 64<<10 {
			return
		}
		_, _ = unwrapApplyPatchArguments(string(arguments))
	})
}
