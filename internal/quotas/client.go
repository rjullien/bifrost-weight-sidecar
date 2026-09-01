// Package quotas reads the OpenCode Go usage API directly and computes the
// weekly/monthly pace used by the weight engine.
package quotas

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// apiURL is variable so tests can redirect requests.
var apiURL = "https://opencode.ai/zen/go/v1/usage"

const (
	maxConcurrentRequests = 4
	maxResponseBody       = 1 << 20 // 1 MiB
)

// Agent is one subscription enriched with local budget computation.
type Agent struct {
	Label   string   `json:"label"`
	Windows []Window `json:"windows"`
	Error   string   `json:"error,omitempty"`
}

// Window is one quota window. Weekly and monthly always carry a valid Budget
// when an Agent has no Error.
type Window struct {
	Name    string  `json:"name"`
	Percent int     `json:"percent"`
	Resets  string  `json:"resets,omitempty"`
	Budget  *Budget `json:"budget,omitempty"`
}

// Budget is the pace computation for a period-based window.
type Budget struct {
	Valid    bool    `json:"valid"`
	DryDays  float64 `json:"dryDays"`
	DaysLeft float64 `json:"daysLeft,omitempty"`
}

type apiResponse struct {
	Usage struct {
		Rolling *windowRaw `json:"rolling"`
		Weekly  *windowRaw `json:"weekly"`
		Monthly *windowRaw `json:"monthly"`
	} `json:"usage"`
}

type windowRaw struct {
	Status   string `json:"status"`
	Percent  *int   `json:"percent"`
	ResetsAt string `json:"resetsAt"`
}

// Client calls the OpenCode Go usage API.
type Client struct {
	http *http.Client
}

// NewClient creates a Client that refuses redirects so credentials are never
// forwarded to a different origin.
func NewClient(timeout time.Duration) *Client {
	return &Client{http: &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// Keys maps a display label (Main, A, N, R, …) to its API key value.
type Keys map[string]string

// Usage is the context-free compatibility wrapper used by package tests.
func (c *Client) Usage(keys Keys) []Agent {
	return c.UsageContext(context.Background(), keys)
}

// UsageContext fetches every subscription with at most four concurrent
// requests. A single timestamp is shared by the whole cycle so equal payloads
// produce equal budgets regardless of response latency.
func (c *Client) UsageContext(ctx context.Context, keys Keys) []Agent {
	labels := make([]string, 0, len(keys))
	for label := range keys {
		labels = append(labels, label)
	}
	sort.Strings(labels)

	agents := make([]Agent, len(labels))
	if len(labels) == 0 {
		return agents
	}

	now := time.Now().UTC()
	jobs := make(chan int)
	workers := min(maxConcurrentRequests, len(labels))
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for i := range jobs {
				label := labels[i]
				a := Agent{Label: label}
				windows, err := c.fetchKey(ctx, keys[label], now)
				if err != nil {
					a.Error = err.Error()
				} else {
					a.Windows = windows
				}
				agents[i] = a
			}
		}()
	}
	for i := range labels {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return agents
}

func (c *Client) fetchKey(ctx context.Context, apiKey string, now time.Time) ([]Window, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("clé OpenCode vide")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
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

	body, err := readResponseBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("lecture réponse API: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("clé invalide ou expirée (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API OpenCode HTTP %d", resp.StatusCode)
	}

	return parseWindowsAt(body, now)
}

// parseWindowsAt fails closed: both policy windows must be present, healthy,
// bounded and active at the cycle timestamp. The private /usage schema is not
// publicly documented, so only the captured production status "ok" is trusted.
func parseWindowsAt(body []byte, now time.Time) ([]Window, error) {
	var resp apiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("réponse API invalide: %w", err)
	}
	if resp.Usage.Monthly == nil || resp.Usage.Weekly == nil {
		return nil, fmt.Errorf("réponse API incomplète: fenêtres monthly et weekly requises")
	}

	monthly, err := periodWindowFrom("Monthly", resp.Usage.Monthly, now)
	if err != nil {
		return nil, err
	}
	weekly, err := periodWindowFrom("Weekly", resp.Usage.Weekly, now)
	if err != nil {
		return nil, err
	}
	windows := []Window{monthly, weekly}

	// Rolling is informational only: expose it when valid, but never let this
	// optional telemetry freeze decisions based on healthy policy windows.
	if resp.Usage.Rolling != nil {
		if rolling, err := rawWindowFrom("Rolling 5h", resp.Usage.Rolling); err == nil {
			windows = append(windows, rolling)
		}
	}
	return windows, nil
}

func rawWindowFrom(name string, raw *windowRaw) (Window, error) {
	if raw.Status != "ok" {
		return Window{}, fmt.Errorf("fenêtre %s invalide: status=%q", name, raw.Status)
	}
	if raw.Percent == nil || *raw.Percent < 0 || *raw.Percent > 100 {
		return Window{}, fmt.Errorf("fenêtre %s invalide: percent doit être compris entre 0 et 100", name)
	}
	reset, err := time.Parse(time.RFC3339Nano, raw.ResetsAt)
	if err != nil || reset.IsZero() {
		return Window{}, fmt.Errorf("fenêtre %s invalide: resetsAt incorrect", name)
	}
	return Window{Name: name, Percent: *raw.Percent, Resets: reset.Format(time.RFC3339)}, nil
}

func periodWindowFrom(name string, raw *windowRaw, now time.Time) (Window, error) {
	window, err := rawWindowFrom(name, raw)
	if err != nil {
		return Window{}, err
	}
	budget := computeBudget(window, now)
	if !budget.Valid {
		return Window{}, fmt.Errorf("fenêtre %s invalide: reset hors période active", name)
	}
	window.Budget = &budget
	return window, nil
}

// computeBudget reproduces the dashboard pace math. A reset is valid only
// while now belongs to the corresponding active weekly/monthly period.
func computeBudget(w Window, now time.Time) Budget {
	reset, err := time.Parse(time.RFC3339, w.Resets)
	if err != nil || reset.IsZero() || w.Percent < 0 || w.Percent > 100 {
		return Budget{}
	}

	var start time.Time
	switch w.Name {
	case "Weekly":
		start = reset.AddDate(0, 0, -7)
	case "Monthly":
		start = monthlyStart(reset)
	default:
		return Budget{}
	}
	if now.Before(start) || !now.Before(reset) {
		return Budget{}
	}

	total := reset.Sub(start)
	elapsed := now.Sub(start)
	if total <= 0 || elapsed < 0 || elapsed >= total {
		return Budget{}
	}

	daysLeft := (total - elapsed).Hours() / 24
	if daysLeft <= 0 || math.IsNaN(daysLeft) || math.IsInf(daysLeft, 0) {
		return Budget{}
	}
	b := Budget{Valid: true, DaysLeft: daysLeft}
	consumed := float64(w.Percent)
	remaining := 100 - consumed

	switch {
	case consumed >= 100:
		b.DryDays = daysLeft
	case elapsed.Hours() > 0 && consumed > 0:
		ratePerDay := consumed / (elapsed.Hours() / 24)
		daysToWall := remaining / ratePerDay
		if daysToWall < daysLeft {
			b.DryDays = daysLeft - daysToWall
		}
	}
	return b
}

func monthlyStart(reset time.Time) time.Time {
	start := reset.AddDate(0, -1, 0)
	if start.Day() != reset.Day() {
		start = start.AddDate(0, 0, -start.Day())
	}
	return start
}

func (a *Agent) WeeklyPercent() int {
	for _, w := range a.Windows {
		if w.Name == "Weekly" {
			return w.Percent
		}
	}
	return -1
}

func (a *Agent) MonthlyDryDays() float64 {
	for _, w := range a.Windows {
		if w.Name == "Monthly" && w.Budget != nil && w.Budget.Valid {
			return w.Budget.DryDays
		}
	}
	return -1
}

func (a *Agent) WeeklyDryDays() float64 {
	for _, w := range a.Windows {
		if w.Name == "Weekly" && w.Budget != nil && w.Budget.Valid {
			return w.Budget.DryDays
		}
	}
	return -1
}

func (a *Agent) MonthlyPercent() int {
	for _, w := range a.Windows {
		if w.Name == "Monthly" {
			return w.Percent
		}
	}
	return -1
}

func (a *Agent) MonthlyDaysLeft() float64 {
	for _, w := range a.Windows {
		if w.Name == "Monthly" && w.Budget != nil && w.Budget.Valid {
			return w.Budget.DaysLeft
		}
	}
	return -1
}

func readResponseBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBody {
		return nil, fmt.Errorf("réponse trop volumineuse (limite %d octets)", maxResponseBody)
	}
	return body, nil
}

// LabelsFromValueRefs maps valid Bifrost env refs to subscription labels.
func LabelsFromValueRefs(refs []string) map[string]bool {
	const prefix = "env.OPENCODE_GO_API_KEY"
	const separator = prefix + "_"
	out := make(map[string]bool)
	for _, ref := range refs {
		switch {
		case ref == prefix:
			out["Main"] = true
		case strings.HasPrefix(ref, separator):
			if label := strings.TrimPrefix(ref, separator); label != "" {
				out[label] = true
			}
		}
	}
	return out
}
