package quotas

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// usagePayload mirrors the live /api/usage payload (windows + budget blocks).
const usagePayload = `[
  {
    "label": "Main",
    "windows": [
      {"name": "Monthly", "kind": "monthly", "status": "ok", "percent": 52,
       "level": "red", "budget": {"valid": true, "periodStart": "2026-08-22T01:23:30.512Z",
       "elapsedPct": 28, "consumedPct": 52, "dryDays": 0, "atCeiling": false}},
      {"name": "Weekly", "kind": "weekly", "status": "ok", "percent": 87,
       "level": "amber", "budget": {"valid": true, "dryDays": 0}},
      {"name": "Rolling 5h", "kind": "rolling", "status": "ok", "percent": 0, "level": "green"}
    ],
    "fetchedAt": "2026-08-30T17:15:39.679Z"
  },
  {"label": "A", "error": "clé injoignable"}
]`

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, 2*time.Second)
}

func TestUsage(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/usage" {
			t.Errorf("path = %q", r.URL.Path)
		}
		fmt.Fprint(w, usagePayload)
	})

	agents, err := c.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(agents))
	}

	main := agents[0]
	if main.Label != "Main" {
		t.Errorf("label = %q, want Main", main.Label)
	}
	if got := main.WeeklyPercent(); got != 87 {
		t.Errorf("weekly = %d, want 87", got)
	}
	if got := main.MonthlyDryDays(); got != 0 {
		t.Errorf("monthly dryDays = %v, want 0", got)
	}

	a := agents[1]
	if a.Error == "" {
		t.Error("A must carry its fetch error")
	}
	if got := a.WeeklyPercent(); got != -1 {
		t.Errorf("A weekly = %d, want -1 (unknown)", got)
	}
	if got := a.MonthlyDryDays(); got != -1 {
		t.Errorf("A monthly dryDays = %v, want -1 (unknown)", got)
	}
}

func TestUsageSurfacesErrors(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := c.Usage(); err == nil {
		t.Fatal("expected an error on HTTP 500")
	}
}
