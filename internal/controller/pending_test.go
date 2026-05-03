package controller

import (
	"testing"
	"time"
)

func TestPendingTracker_AddRemoveCount(t *testing.T) {
	p := NewPendingTracker(time.Hour)
	if !p.Add("app", "n1") {
		t.Fatal("Add returned false")
	}
	if p.Add("app", "n1") {
		t.Fatal("duplicate Add should return false")
	}
	if got := p.Count("app"); got != 1 {
		t.Errorf("Count(app) = %d, want 1", got)
	}
	if got := p.Count("media"); got != 0 {
		t.Errorf("Count(media) = %d, want 0", got)
	}
	p.Remove("n1")
	if got := p.Count("app"); got != 0 {
		t.Errorf("Count(app) after remove = %d, want 0", got)
	}
}

func TestPendingTracker_TTLExpiry(t *testing.T) {
	p := NewPendingTracker(50 * time.Millisecond)
	now := time.Now()
	p.now = func() time.Time { return now }
	p.Add("app", "n1")
	now = now.Add(time.Hour)
	if got := p.Count("app"); got != 0 {
		t.Errorf("expected expiry, Count = %d", got)
	}
}
