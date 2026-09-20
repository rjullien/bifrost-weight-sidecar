package quotas

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// apiPayload mirrors the live OpenCode Go /v1/usage payload while keeping
// resets inside the active periods at test execution time.
func apiPayload() string {
	now := time.Now().UTC()
	return fmt.Sprintf(`{
  "usage": {
    "rolling": {"status": "ok", "percent": 0, "resetsAt": %q},
    "weekly":  {"status": "ok", "percent": 87, "resetsAt": %q},
    "monthly": {"status": "ok", "percent": 52, "resetsAt": %q}
  }
}`,
		now.Add(4*time.Hour).Format(time.RFC3339Nano),
		now.Add(3*24*time.Hour).Format(time.RFC3339Nano),
		now.Add(10*24*time.Hour).Format(time.RFC3339Nano),
	)
}

func TestUsageDirectAPI(t *testing.T) {
	apiURL = "http://unused.local" // rewritten by the test server below
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zen/go/v1/usage" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("missing Bearer header")
		}
		fmt.Fprint(w, apiPayload())
	}))
	defer srv.Close()
	apiURL = srv.URL + "/zen/go/v1/usage"

	c := NewClient(2 * time.Second)
	agents := c.Usage(Keys{"Main": "key-main", "A": "key-bad"})
	if len(agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(agents))
	}

	var main *Agent
	for i := range agents {
		if agents[i].Label == "Main" {
			main = &agents[i]
		}
	}
	if main == nil {
		t.Fatal("Main agent missing")
	}
	if got := main.WeeklyPercent(); got != 87 {
		t.Errorf("weekly = %d, want 87", got)
	}
	if got := main.MonthlyDryDays(); got < 0 {
		t.Errorf("monthly dryDays = %v, want >= 0", got)
	}
	if !main.hasWindow("Rolling 5h") {
		t.Error("Rolling 5h window missing")
	}
}

func TestUsageKeyError(t *testing.T) {
	apiURL = "http://unused.local"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	apiURL = srv.URL + "/zen/go/v1/usage"

	c := NewClient(2 * time.Second)
	agents := c.Usage(Keys{"Main": "bad"})
	if len(agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(agents))
	}
	if agents[0].Error == "" {
		t.Error("expected a fetch error")
	}
	if got := agents[0].WeeklyPercent(); got != -1 {
		t.Errorf("weekly = %d, want -1 (unknown)", got)
	}
}

func TestComputeBudgetMonthly(t *testing.T) {
	// Reset 2026-09-22, now 2026-09-10, 52% consumed → still short of the
	// J−1 wall at current pace (dryDays 0), with or without burn lead.
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	w := Window{Name: "Monthly", Percent: 52, Resets: "2026-09-22T01:23:30.000Z"}
	b := computeBudget(w, now, DefaultBurnLead)
	if !b.Valid {
		t.Fatal("budget should be valid")
	}
	if b.DryDays != 0 {
		t.Errorf("dryDays = %v, want 0", b.DryDays)
	}

	// At the ceiling: 100% → dry until reset.
	w2 := Window{Name: "Monthly", Percent: 100, Resets: "2026-09-22T01:23:30.000Z"}
	b2 := computeBudget(w2, now, DefaultBurnLead)
	if !b2.Valid || b2.DryDays <= 0 {
		t.Errorf("ceiling dryDays = %v (valid=%v), want >0", b2.DryDays, b2.Valid)
	}
}

// TestComputeBudgetMonthlyJMinus1Wall covers the Nicole-style what-if: a key
// projected to hit 100% before the anniversary reset, but only after the J−1
// burn wall, must report DryDays == 0 (under-burner). Far from reset with
// comfortable pace stays on-track (DryDays > 0).
func TestComputeBudgetMonthlyJMinus1Wall(t *testing.T) {
	tests := []struct {
		name     string
		now      time.Time
		percent  int
		resets   string
		burnLead time.Duration
		wantDry  string // "under" (0) or "ontrack" (>0)
	}{
		{
			// Live-shaped: 2026-09-20 21:36 UTC, reset 2026-09-22 00:00,
			// ~1.1d left, 98% consumed. Against resetsAt DryDays≈0.5 (on-track);
			// against J−1 wall → under-burner.
			name:     "nicole_98pct_1.1d_vs_J-1",
			now:      time.Date(2026, 9, 20, 21, 36, 0, 0, time.UTC),
			percent:  98,
			resets:   "2026-09-22T00:00:00.000Z",
			burnLead: DefaultBurnLead,
			wantDry:  "under",
		},
		{
			// Same snapshot with burn lead disabled → still on-track vs reset.
			name:     "nicole_ontrack_without_lead",
			now:      time.Date(2026, 9, 20, 21, 36, 0, 0, time.UTC),
			percent:  98,
			resets:   "2026-09-22T00:00:00.000Z",
			burnLead: 0,
			wantDry:  "ontrack",
		},
		{
			// Mid-cycle, pace finishes well before J−1 → on-track.
			name:     "far_from_reset_ontrack",
			now:      time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
			percent:  70,
			resets:   "2026-09-22T00:00:00.000Z",
			burnLead: DefaultBurnLead,
			wantDry:  "ontrack",
		},
		{
			// Already inside the final 24h before reset with remaining → under.
			name:     "past_wall_remaining_under",
			now:      time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
			percent:  95,
			resets:   "2026-09-22T00:00:00.000Z",
			burnLead: DefaultBurnLead,
			wantDry:  "under",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := Window{Name: "Monthly", Percent: tt.percent, Resets: tt.resets}
			b := computeBudget(w, tt.now, tt.burnLead)
			if !b.Valid {
				t.Fatalf("budget invalid")
			}
			if b.DaysLeft <= 0 {
				t.Fatalf("DaysLeft = %v, want >0 until real reset", b.DaysLeft)
			}
			switch tt.wantDry {
			case "under":
				if b.DryDays != 0 {
					t.Errorf("DryDays = %v, want 0 (under-burner vs J−1 wall)", b.DryDays)
				}
			case "ontrack":
				if b.DryDays <= 0 {
					t.Errorf("DryDays = %v, want >0 (on-track for burn wall)", b.DryDays)
				}
			default:
				t.Fatalf("unknown wantDry %q", tt.wantDry)
			}
		})
	}
}

func TestLabelsFromValueRefs(t *testing.T) {
	out := LabelsFromValueRefs([]string{
		"env.OPENCODE_GO_API_KEY",
		"env.OPENCODE_GO_API_KEY_A",
		"env.OPENCODE_GO_API_KEY_N",
	})
	if !out["Main"] || !out["A"] || !out["N"] {
		t.Errorf("labels = %v, want Main+A+N", out)
	}
	if out["R"] {
		t.Error("R must not be present")
	}
}

// hasWindow is a test helper.
func (a *Agent) hasWindow(name string) bool {
	for _, w := range a.Windows {
		if w.Name == name {
			return true
		}
	}
	return false
}
func TestNewAccessors(t *testing.T) {
	agents := []Agent{
		{Label: "X", Windows: []Window{
			{Name: "Monthly", Percent: 80, Budget: &Budget{Valid: true, DryDays: 0, DaysLeft: 5}},
			{Name: "Weekly", Percent: 90, Budget: &Budget{Valid: true, DryDays: 1.5}},
		}},
	}
	a := &agents[0]
	if got := a.MonthlyPercent(); got != 80 {
		t.Errorf("MonthlyPercent = %d, want 80", got)
	}
	if got := a.MonthlyDaysLeft(); got != 5 {
		t.Errorf("MonthlyDaysLeft = %v, want 5", got)
	}
	if got := a.WeeklyDryDays(); got != 1.5 {
		t.Errorf("WeeklyDryDays = %v, want 1.5", got)
	}
}

func TestRollingPercent(t *testing.T) {
	// Agent with a rolling window at 99%.
	withRolling := &Agent{Label: "X", Windows: []Window{
		{Name: "Monthly", Percent: 50},
		{Name: "Rolling 5h", Percent: 99},
	}}
	if got := withRolling.RollingPercent(); got != 99 {
		t.Errorf("RollingPercent = %d, want 99", got)
	}

	// Agent with no rolling window → -1 (unknown, must not evict).
	noRolling := &Agent{Label: "Y", Windows: []Window{{Name: "Monthly", Percent: 50}}}
	if got := noRolling.RollingPercent(); got != -1 {
		t.Errorf("RollingPercent = %d, want -1 (no rolling window)", got)
	}
}
