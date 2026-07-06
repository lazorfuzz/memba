package api

import "testing"

func TestRateLimiterBudgetAndIsolation(t *testing.T) {
	rl := newRateLimiter(60) // 60/min budget, burst 60
	for i := 0; i < 60; i++ {
		if !rl.allow("t/a") {
			t.Fatalf("request %d within burst must pass", i)
		}
	}
	if rl.allow("t/a") {
		t.Fatal("61st immediate request must be limited")
	}
	// Other principals are unaffected.
	if !rl.allow("t/b") {
		t.Fatal("independent principal must not be limited")
	}
	// Disabled limiter always allows.
	var off *rateLimiter
	if !off.allow("t/a") {
		t.Fatal("nil limiter must be a no-op")
	}
}
