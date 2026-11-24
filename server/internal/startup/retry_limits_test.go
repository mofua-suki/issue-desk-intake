package startup

import "testing"

func TestRetryLimitsDefaultUsageLimitWhenNothingIsConfigured(t *testing.T) {
	got := (&App{}).retryLimits(map[string]any{"implementationRunLimit": float64(3)})
	if got.run != 3 || got.usage != 8000000 {
		t.Fatalf("設定値を優先し、使用量の上限は既定の800万にします: %+v", got)
	}
	if (&App{}).retryLimits(map[string]any{}).usage <= 0 {
		t.Fatal("上限が0のままだと、すべての再試行が「累計使用量が未知」で断られます")
	}
}
