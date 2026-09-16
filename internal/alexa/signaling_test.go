package alexa

import "testing"

func TestPlainIDAcceptsAlexaShapesAndNothingElse(t *testing.T) {
	for _, ok := range []string{"0028792d-d00f-4618-97f3-27d70bbea1c2", "abc_DEF-123", "x"} {
		if !plainID(ok) {
			t.Fatalf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{"", "../etc", "a/b", `a\b`, "a b", "a.b", "a\n", "ـ", string(make([]byte, 129))} {
		if plainID(bad) {
			t.Fatalf("%q should be refused", bad)
		}
	}
}
