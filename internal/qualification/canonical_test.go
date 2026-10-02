package qualification

import (
	"encoding/json"
	"testing"
)

func TestCanonicalJSONV1FixedVector(t *testing.T) {
	value := map[string]any{
		"z": []any{map[string]any{"b": true, "a": 1}},
		"a": "café <&>",
		"floats": []json.Number{
			"1.0",
			"1e-7",
			"1e20",
			"1e21",
			"-0.0",
			"333333333.33333329",
			"9223372036854775807",
			"9007199254740993",
		},
	}
	want := `{"a":"café <&>","floats":[1,1e-7,100000000000000000000,1e+21,0,333333333.3333333,9223372036854775807,9007199254740993],"z":[{"a":1,"b":true}]}`
	got, err := CanonicalJSON(value)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if string(got) != want {
		t.Fatalf("CanonicalJSON = %s, want %s", got, want)
	}
}

func TestCanonicalJSONRejectsBinary64Underflow(t *testing.T) {
	if _, err := CanonicalJSON(json.Number("1e-9999")); err == nil {
		t.Fatal("CanonicalJSON accepted a nonzero number that underflows binary64")
	}
}
