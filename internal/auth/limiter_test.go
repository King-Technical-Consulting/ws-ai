package auth

import (
	"testing"
	"time"
)

func TestSendLimiterPerAddress(t *testing.T) {
	l := newSendLimiter(3, 100, 15*time.Minute)
	clock := time.Now()
	l.now = func() time.Time { return clock }
	for i := 0; i < 3; i++ {
		if !l.Allow("a@example.com") {
			t.Fatalf("attempt %d refused", i+1)
		}
	}
	if l.Allow("a@example.com") {
		t.Fatal("a fourth attempt in the window was allowed")
	}
	if !l.Allow("b@example.com") {
		t.Fatal("another address was held up by the first")
	}
	// The window slides: after it passes, the address may ask again.
	clock = clock.Add(15*time.Minute + time.Second)
	if !l.Allow("a@example.com") {
		t.Fatal("still refused after the window")
	}
}

func TestSendLimiterOverall(t *testing.T) {
	l := newSendLimiter(3, 5, time.Hour)
	for i := 0; i < 5; i++ {
		if !l.Allow(string(rune('a' + i))) {
			t.Fatalf("attempt %d refused", i+1)
		}
	}
	if l.Allow("z") {
		t.Fatal("the overall cap did not hold")
	}
}

func TestSendLimiterRefusalsDoNotExtendTheBlock(t *testing.T) {
	l := newSendLimiter(1, 100, 10*time.Minute)
	clock := time.Now()
	l.now = func() time.Time { return clock }
	l.Allow("a")
	for i := 0; i < 18; i++ { // a flood, every 30 seconds, for 9 minutes
		clock = clock.Add(30 * time.Second)
		l.Allow("a")
	}
	// Past 10 minutes from the only recorded attempt, the owner can ask again.
	clock = clock.Add(90 * time.Second)
	if !l.Allow("a") {
		t.Fatal("a flood kept the window shut for the owner")
	}
}

func TestSendLimiterForgetsIdleAddresses(t *testing.T) {
	l := newSendLimiter(3, 1000, time.Minute)
	clock := time.Now()
	l.now = func() time.Time { return clock }
	for i := 0; i < 50; i++ {
		l.Allow(string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}
	clock = clock.Add(2 * time.Minute)
	l.Allow("fresh")
	if n := len(l.byKey); n != 1 { // only the fresh key remains
		t.Fatalf("%d idle addresses kept", n)
	}
}

func TestSendLimiterReportsOneRefusalPerStreak(t *testing.T) {
	l := newSendLimiter(1, 100, 10*time.Minute)
	clock := time.Now()
	l.now = func() time.Time { return clock }
	if ok, _ := l.AllowReport("a"); !ok {
		t.Fatal("first attempt refused")
	}
	ok, first := l.AllowReport("a")
	if ok || !first {
		t.Fatalf("first refusal: ok=%v first=%v", ok, first)
	}
	for i := 0; i < 5; i++ {
		if ok, first := l.AllowReport("a"); ok || first {
			t.Fatalf("later refusal %d: ok=%v first=%v", i, ok, first)
		}
	}
	// Once allowed again, a new streak is reported again.
	clock = clock.Add(11 * time.Minute)
	if ok, _ := l.AllowReport("a"); !ok {
		t.Fatal("not allowed after the window")
	}
	if ok, first := l.AllowReport("a"); ok || !first {
		t.Fatalf("new streak: ok=%v first=%v", ok, first)
	}
}
