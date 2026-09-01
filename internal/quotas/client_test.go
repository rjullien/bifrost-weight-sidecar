package quotas

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// apiPayload mirrors the live OpenCode Go /v1/usage payload.
const apiPayload = `{
  "usage": {
    "rolling": {"status": "ok", "percent": 0, "resetsAt": "2026-08-30T22:15:39.000Z"},
    "weekly":  {"status": "ok", "percent": 87, "resetsAt": "2026-08-31T00:00:00.000Z"},
    "monthly": {"status": "ok", "percent": 52, "resetsAt": "2026-09-22T01:23:30.000Z"}
  }
}`

func TestUsageDirectAPI(t *testing.T) {
	apiURL = "http://unused.local" // rewritten by the test server below
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zen/go/v1/usage" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("missing Bearer header")
		}
		fmt.Fprint(w, apiPayload)
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
	// Reset 2026-09-22, now 2026-09-10, 52% consumed → budget holds (dryDays 0).
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	w := Window{Name: "Monthly", Percent: 52, Resets: "2026-09-22T01:23:30.000Z"}
	b := computeBudget(w, now)
	if !b.Valid {
		t.Fatal("budget should be valid")
	}
	if b.DryDays != 0 {
		t.Errorf("dryDays = %v, want 0", b.DryDays)
	}

	// At the ceiling: 100% → dry until reset.
	w2 := Window{Name: "Monthly", Percent: 100, Resets: "2026-09-22T01:23:30.000Z"}
	b2 := computeBudget(w2, now)
	if !b2.Valid || b2.DryDays <= 0 {
		t.Errorf("ceiling dryDays = %v (valid=%v), want >0", b2.DryDays, b2.Valid)
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
