package envx

import (
	"testing"
	"time"
)

func TestHelpers(t *testing.T) {
	t.Setenv("ENVX_S", "  v  ")
	t.Setenv("ENVX_I", " 42 ")
	t.Setenv("ENVX_BAD", "x")
	t.Setenv("ENVX_D", "3s")
	t.Setenv("ENVX_NEG", "-1s")
	if got := String("ENVX_S", "f"); got != "v" {
		t.Fatalf("String=%q", got)
	}
	if got := String("ENVX_MISSING", "f"); got != "f" {
		t.Fatalf("String fallback=%q", got)
	}
	if got := Int("ENVX_I", 1); got != 42 {
		t.Fatalf("Int=%d", got)
	}
	if got := Int("ENVX_BAD", 7); got != 7 {
		t.Fatalf("Int fallback=%d", got)
	}
	if got := Duration("ENVX_D", time.Second); got != 3*time.Second {
		t.Fatalf("Duration=%s", got)
	}
	if got := Duration("ENVX_NEG", time.Second); got != time.Second {
		t.Fatalf("Duration non-positive must fall back, got %s", got)
	}
}
