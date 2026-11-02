package ratelimit

import (
	"strconv"
	"testing"
	"time"
)

func TestAllow(t *testing.T) {
	l := New(3)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if !l.Allow("alice") {
			t.Fatalf("call %d denied within burst", i)
		}
	}
	if l.Allow("alice") {
		t.Fatal("call beyond burst allowed")
	}
	if !l.Allow("bob") {
		t.Fatal("keys must be independent")
	}
	now = now.Add(20 * time.Second) // 3/min -> one token per 20s
	if !l.Allow("alice") {
		t.Fatal("token not replenished")
	}
}

func TestEviction(t *testing.T) {
	l := New(1)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	for i := 0; i <= 1000; i++ {
		l.Allow(strconv.Itoa(i))
	}
	now = now.Add(time.Hour)
	l.Allow("fresh")
	if len(l.entries) != 1 {
		t.Fatalf("idle entries not evicted: %d", len(l.entries))
	}
}
