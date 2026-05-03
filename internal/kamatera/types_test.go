package kamatera

import "testing"

func TestCommandStatus_IsTerminal(t *testing.T) {
	cases := []struct {
		status CommandStatus
		want   bool
	}{
		{StatusInitializing, false},
		{StatusProgress, false},
		{StatusStarted, false},
		{StatusComplete, true},
		{StatusError, true},
		{StatusCancelled, true},
		{CommandStatus("unknown-status"), false},
		{CommandStatus(""), false},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			if got := tc.status.IsTerminal(); got != tc.want {
				t.Errorf("IsTerminal(%q) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

func TestCommandStatus_IsFailure(t *testing.T) {
	cases := []struct {
		status CommandStatus
		want   bool
	}{
		{StatusInitializing, false},
		{StatusProgress, false},
		{StatusStarted, false},
		{StatusComplete, false},
		{StatusError, true},
		{StatusCancelled, true},
		{CommandStatus("unknown-status"), false},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			if got := tc.status.IsFailure(); got != tc.want {
				t.Errorf("IsFailure(%q) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// TestCommandStatus_TerminalImpliesNotProgressing locks in the invariant that
// a status can't be both terminal AND in-progress (catches future
// regressions where someone adds a state to one set but forgets the other).
func TestCommandStatus_TerminalImpliesNotProgressing(t *testing.T) {
	progressing := []CommandStatus{StatusInitializing, StatusProgress, StatusStarted}
	for _, s := range progressing {
		if s.IsTerminal() {
			t.Errorf("%q should not be terminal", s)
		}
	}
	terminals := []CommandStatus{StatusComplete, StatusError, StatusCancelled}
	for _, s := range terminals {
		if !s.IsTerminal() {
			t.Errorf("%q should be terminal", s)
		}
	}
}
