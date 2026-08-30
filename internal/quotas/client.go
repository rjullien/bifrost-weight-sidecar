// Package quotas reads the OpenCode Go usage dashboard
// (rjullien/opencode-usage-tracker, endpoint /api/usage). It yields the quota
// position of every subscription, keyed by display label (Main, A, N, R, …).
package quotas

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Agent is one subscription as exposed by the dashboard.
type Agent struct {
	Label   string   `json:"label"`
	Windows []Window `json:"windows"`
	Error   string   `json:"error,omitempty"`
}

// Window is one quota window of an agent. Budget is present on the windows
// the dashboard grades on pace (weekly and monthly); rolling carries none.
type Window struct {
	Name    string  `json:"name"`
	Percent int     `json:"percent"`
	Budget  *Budget `json:"budget"`
}

// Budget is the dashboard's pace computation for a period-based window.
type Budget struct {
	Valid   bool    `json:"valid"`
	DryDays float64 `json:"dryDays"`
}

// Client calls the OpenCode Go usage dashboard.
type Client struct {
	http    *http.Client
	baseURL string
}

// NewClient creates a Client for the given dashboard base URL.
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		http:    &http.Client{Timeout: timeout},
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

// Usage returns the current quota position of every subscription.
func (c *Client) Usage() ([]Agent, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/api/usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dashboard injoignable: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("lecture réponse dashboard: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dashboard HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var agents []Agent
	if err := json.Unmarshal(body, &agents); err != nil {
		return nil, fmt.Errorf("réponse dashboard invalide: %w", err)
	}
	return agents, nil
}

// WeeklyPercent returns the consumption of the "Weekly" window, or -1 when the
// agent carries no such window (or is in error).
func (a *Agent) WeeklyPercent() int {
	for _, w := range a.Windows {
		if w.Name == "Weekly" {
			return w.Percent
		}
	}
	return -1
}

// MonthlyDryDays returns the number of days the monthly quota would sit at the
// ceiling (0 = the budget holds), or -1 when unknown.
func (a *Agent) MonthlyDryDays() float64 {
	for _, w := range a.Windows {
		if w.Name == "Monthly" && w.Budget != nil && w.Budget.Valid {
			return w.Budget.DryDays
		}
	}
	return -1
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
