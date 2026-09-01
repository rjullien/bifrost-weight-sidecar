// Package quotas reads the OpenCode Go usage API directly
// (https://opencode.ai/zen/go/v1/usage) with the subscription keys injected in
// the sidecar environment. It yields the quota position of every subscription,
// keyed by display label (Main, A, N, R, …).
//
// The dashboard (opencode-usage-tracker) is deliberately NOT used: the sidecar
// must stay decoupled from it (Baptiste, review PR #166). The pace/math logic
// is reproduced here so the sidecar is self-contained.
package quotas

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// apiURL is the OpenCode Go usage endpoint. Variable so tests can redirect it.
var apiURL = "https://opencode.ai/zen/go/v1/usage"

// Agent is one subscription as exposed by the OpenCode Go API, enriched with
// the local budget computation.
type Agent struct {
	Label   string   `json:"label"`
	Windows []Window `json:"windows"`
	Error   string   `json:"error,omitempty"`
}

// Window is one quota window. Budget is present on the windows the engine
// grades on pace (weekly and monthly); rolling carries none.
type Window struct {
	Name    string  `json:"name"`
	Percent int     `json:"percent"`
	Resets  string  `json:"resets,omitempty"`
	Budget  *Budget `json:"budget,omitempty"`
}

// Budget is the pace computation for a period-based window, reproduced from
// the dashboard's internal/opencode/budget.go.
type Budget struct {
	Valid    bool    `json:"valid"`
	DryDays  float64 `json:"dryDays"`
	DaysLeft float64 `json:"daysLeft,omitempty"`
}

// apiResponse matches the real OpenCode Go API response format.
type apiResponse struct {
	Usage struct {
		Rolling *windowRaw `json:"rolling"`
		Weekly  *windowRaw `json:"weekly"`
		Monthly *windowRaw `json:"monthly"`
	} `json:"usage"`
}

type windowRaw struct {
	Status   string `json:"status"`
	Percent  int    `json:"percent"`
	ResetsAt string `json:"resetsAt"`
}

// Client calls the OpenCode Go usage API for a set of keys.
type Client struct {
	http *http.Client
}

// NewClient creates a Client with the given timeout.
func NewClient(timeout time.Duration) *Client {
	return &Client{http: &http.Client{Timeout: timeout}}
}

// Keys maps a display label (Main, A, N, R, …) to its API key value.
type Keys map[string]string

// Usage fetches the quota position of every given key, directly from the
// OpenCode Go API. A key that fails to fetch yields an Agent carrying Error.
func (c *Client) Usage(keys Keys) []Agent {
	var agents []Agent
	for label, key := range keys {
		a := Agent{Label: label}
		windows, err := c.fetchKey(key)
		if err != nil {
			a.Error = err.Error()
		} else {
			a.Windows = windows
		}
		agents = append(agents, a)
	}
	return agents
}

func (c *Client) fetchKey(apiKey string) ([]Window, error) {
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "bifrost-weight-sidecar/2.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API OpenCode injoignable: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("lecture réponse API: %w", err)
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("clé invalide ou expirée (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API OpenCode HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	return parseWindows(body)
}

func parseWindows(body []byte) ([]Window, error) {
	var resp apiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("réponse API invalide: %w", err)
	}

	now := time.Now().UTC()
	var windows []Window

	// Monthly first: it is the window that actually constrains the month.
	if w := resp.Usage.Monthly; w != nil {
		windows = append(windows, windowFrom("Monthly", w, now))
	}
	if w := resp.Usage.Weekly; w != nil {
		windows = append(windows, windowFrom("Weekly", w, now))
	}
	if w := resp.Usage.Rolling; w != nil {
		windows = append(windows, Window{Name: "Rolling 5h", Percent: w.Percent})
	}

	if len(windows) == 0 {
		return nil, fmt.Errorf("aucune fenêtre trouvée: %s", truncate(string(body), 300))
	}
	return windows, nil
}

func windowFrom(name string, raw *windowRaw, now time.Time) Window {
	w := Window{Name: name, Percent: raw.Percent}
	if t, err := time.Parse(time.RFC3339Nano, raw.ResetsAt); err == nil {
		w.Resets = t.Format(time.RFC3339)
	} else if t, err := time.Parse(time.RFC3339, raw.ResetsAt); err == nil {
		w.Resets = t.Format(time.RFC3339)
	}

	if b := computeBudget(w, now); b.Valid {
		w.Budget = &b
	}
	return w
}

// computeBudget reproduces the dashboard's ComputeBudget: the weekly window
// resets every 7 days; the monthly window is a subscription anniversary
// (1 calendar month before the reset). DryDays > 0 means the quota is
// projected to sit at the ceiling before the reset.
func computeBudget(w Window, now time.Time) Budget {
	b := Budget{Valid: false}

	reset, err := time.Parse(time.RFC3339, w.Resets)
	if err != nil || reset.IsZero() {
		return b
	}

	var start time.Time
	switch w.Name {
	case "Weekly":
		start = reset.AddDate(0, 0, -7)
	case "Monthly":
		start = monthlyStart(reset)
	default:
		return b // rolling: no pace maths
	}

	total := reset.Sub(start)
	if total <= 0 {
		return b
	}
	elapsed := now.Sub(start)
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > total {
		elapsed = total
	}

	b.Valid = true
	daysLeft := math.Max(0, total.Hours()/24-elapsed.Hours()/24)
	b.DaysLeft = daysLeft

	consumed := float64(w.Percent)
	remaining := math.Max(0, 100-consumed)

	switch {
	case consumed >= 100:
		b.DryDays = daysLeft
	case elapsed.Hours()/24 > 0:
		ratePerDay := consumed / (elapsed.Hours() / 24)
		if daysToWall := remaining / ratePerDay; daysToWall < daysLeft {
			b.DryDays = daysLeft - daysToWall
		}
	}
	return b
}

// monthlyStart steps back one calendar month from the reset instant, keeping
// the subscription anniversary (same clamp as the dashboard).
func monthlyStart(reset time.Time) time.Time {
	start := reset.AddDate(0, -1, 0)
	if start.Day() != reset.Day() {
		start = start.AddDate(0, 0, -start.Day())
	}
	return start
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

// WeeklyDryDays returns the number of days the weekly quota would sit at the
// ceiling (0 = the budget holds), or -1 when unknown. The weekly is a
// blocker, not a loss: when it hits the ceiling the key stops serving, so the
// engine anticipates it.
func (a *Agent) WeeklyDryDays() float64 {
	for _, w := range a.Windows {
		if w.Name == "Weekly" && w.Budget != nil && w.Budget.Valid {
			return w.Budget.DryDays
		}
	}
	return -1
}

// MonthlyPercent returns the raw monthly consumption (0-100), or -1 when the
// agent carries no monthly window (or is in error).
func (a *Agent) MonthlyPercent() int {
	for _, w := range a.Windows {
		if w.Name == "Monthly" {
			return w.Percent
		}
	}
	return -1
}

// MonthlyDaysLeft returns the number of days remaining until the monthly
// reset (anniversary), or -1 when unknown.
func (a *Agent) MonthlyDaysLeft() float64 {
	for _, w := range a.Windows {
		if w.Name == "Monthly" && w.Budget != nil && w.Budget.Valid {
			return w.Budget.DaysLeft
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

// LabelsFromValueRefs maps a Bifrost key value ref (e.g.
// "env.OPENCODE_GO_API_KEY_A") to the display label, mirroring the dashboard:
// "env.OPENCODE_GO_API_KEY" is "Main", a suffix is the label.
func LabelsFromValueRefs(refs []string) map[string]bool {
	const prefix = "env.OPENCODE_GO_API_KEY"
	out := make(map[string]bool)
	for _, ref := range refs {
		if !strings.HasPrefix(ref, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(ref, prefix)
		if suffix == "" {
			out["Main"] = true
		} else {
			out[strings.TrimPrefix(suffix, "_")] = true
		}
	}
	return out
}
