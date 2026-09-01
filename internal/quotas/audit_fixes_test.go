package quotas

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func withTestAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	previous := apiURL
	srv := httptest.NewServer(handler)
	apiURL = srv.URL + "/zen/go/v1/usage"
	t.Cleanup(func() {
		apiURL = previous
		srv.Close()
	})
}

func TestUsageTrimsAPIKey(t *testing.T) {
	withTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q, want trimmed bearer", got)
		}
		fmt.Fprint(w, apiPayload())
	})

	agents := NewClient(2 * time.Second).Usage(Keys{"Main": " secret\n"})
	if len(agents) != 1 || agents[0].Error != "" {
		t.Fatalf("agents = %+v, want one successful agent", agents)
	}
}

func TestUsageRejectsOversizedResponse(t *testing.T) {
	withTestAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", maxResponseBody+1))
	})

	agents := NewClient(2 * time.Second).Usage(Keys{"Main": "secret"})
	if len(agents) != 1 || !strings.Contains(agents[0].Error, "trop volumineuse") {
		t.Fatalf("agents = %+v, want oversized-response error", agents)
	}
}

func TestUsageBoundsConcurrency(t *testing.T) {
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()

	var active atomic.Int32
	var peak atomic.Int32
	withTestAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		fmt.Fprint(w, apiPayload())
	})

	keys := Keys{}
	for _, label := range []string{"H", "G", "F", "E", "D", "C", "B", "A"} {
		keys[label] = "secret-" + label
	}
	done := make(chan []Agent, 1)
	go func() {
		done <- NewClient(2 * time.Second).Usage(keys)
	}()

	for range maxConcurrentRequests {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start concurrently")
		}
	}
	select {
	case <-started:
		t.Fatalf("more than %d requests started before a worker was released", maxConcurrentRequests)
	case <-time.After(25 * time.Millisecond):
	}

	releaseAll()
	agents := <-done
	if got := peak.Load(); got != maxConcurrentRequests {
		t.Fatalf("peak concurrency = %d, want %d", got, maxConcurrentRequests)
	}
	if len(agents) != len(keys) {
		t.Fatalf("agents = %d, want %d", len(agents), len(keys))
	}
	for i := 1; i < len(agents); i++ {
		if agents[i-1].Label > agents[i].Label {
			t.Fatalf("agents not sorted: %+v", agents)
		}
	}
}

func TestLabelsFromValueRefsRejectsMalformedReferences(t *testing.T) {
	labels := LabelsFromValueRefs([]string{
		"env.OPENCODE_GO_API_KEYA",
		"env.OPENCODE_GO_API_KEY_",
	})
	if len(labels) != 0 {
		t.Fatalf("labels = %v, want none", labels)
	}
}

func intPointer(value int) *int { return &value }

func validRawWindows(now time.Time) (*windowRaw, *windowRaw) {
	monthly := &windowRaw{
		Status: "ok", Percent: intPointer(52),
		ResetsAt: now.Add(10 * 24 * time.Hour).Format(time.RFC3339Nano),
	}
	weekly := &windowRaw{
		Status: "ok", Percent: intPointer(87),
		ResetsAt: now.Add(3 * 24 * time.Hour).Format(time.RFC3339Nano),
	}
	return monthly, weekly
}

func marshalUsage(t *testing.T, monthly, weekly, rolling *windowRaw) []byte {
	t.Helper()
	payload := apiResponse{}
	payload.Usage.Monthly = monthly
	payload.Usage.Weekly = weekly
	payload.Usage.Rolling = rolling
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal usage: %v", err)
	}
	return body
}

func TestParseWindowsFailsClosedOnIncompleteOrInvalidPolicyWindows(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	validMonthly, validWeekly := validRawWindows(now)

	tests := []struct {
		name    string
		mutate  func(*windowRaw, *windowRaw) (*windowRaw, *windowRaw)
		wantErr string
	}{
		{"missing monthly", func(_ *windowRaw, w *windowRaw) (*windowRaw, *windowRaw) { return nil, w }, "incomplète"},
		{"missing weekly", func(m *windowRaw, _ *windowRaw) (*windowRaw, *windowRaw) { return m, nil }, "incomplète"},
		{"status error", func(m *windowRaw, w *windowRaw) (*windowRaw, *windowRaw) { w.Status = "error"; return m, w }, "status"},
		{"percent absent", func(m *windowRaw, w *windowRaw) (*windowRaw, *windowRaw) { m.Percent = nil; return m, w }, "percent"},
		{"percent negative", func(m *windowRaw, w *windowRaw) (*windowRaw, *windowRaw) { m.Percent = intPointer(-1); return m, w }, "percent"},
		{"percent above ceiling", func(m *windowRaw, w *windowRaw) (*windowRaw, *windowRaw) { w.Percent = intPointer(101); return m, w }, "percent"},
		{"past weekly reset", func(m *windowRaw, w *windowRaw) (*windowRaw, *windowRaw) {
			w.ResetsAt = now.Add(-time.Hour).Format(time.RFC3339)
			return m, w
		}, "période active"},
		{"future weekly period", func(m *windowRaw, w *windowRaw) (*windowRaw, *windowRaw) {
			w.ResetsAt = now.Add(8 * 24 * time.Hour).Format(time.RFC3339)
			return m, w
		}, "période active"},
		{"future monthly period", func(m *windowRaw, w *windowRaw) (*windowRaw, *windowRaw) {
			m.ResetsAt = now.AddDate(0, 2, 0).Format(time.RFC3339)
			return m, w
		}, "période active"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			monthlyCopy, weeklyCopy := *validMonthly, *validWeekly
			monthly, weekly := test.mutate(&monthlyCopy, &weeklyCopy)
			_, err := parseWindowsAt(marshalUsage(t, monthly, weekly, nil), now)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("parseWindowsAt error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestUsageRefusesRedirectWithoutForwardingSecret(t *testing.T) {
	redirected := 0
	withTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			redirected++
			if r.Header.Get("Authorization") != "" {
				t.Error("Authorization forwarded to redirect target")
			}
			fmt.Fprint(w, apiPayload())
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusFound)
	})

	agents := NewClient(2 * time.Second).Usage(Keys{"Main": "secret"})
	if len(agents) != 1 || !strings.Contains(agents[0].Error, "HTTP 302") {
		t.Fatalf("agents = %+v, want redirect refusal", agents)
	}
	if redirected != 0 {
		t.Fatalf("redirect target called %d time(s), want 0", redirected)
	}
}

func TestUsageDoesNotExposeRemoteErrorBody(t *testing.T) {
	withTestAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `internal secret: token-123`)
	})

	agent := NewClient(2 * time.Second).Usage(Keys{"Main": "secret"})[0]
	if !strings.Contains(agent.Error, "HTTP 500") {
		t.Fatalf("error = %q, want HTTP 500", agent.Error)
	}
	if strings.Contains(agent.Error, "token-123") {
		t.Fatalf("remote response body leaked: %q", agent.Error)
	}
}

func TestUsageContextCancellation(t *testing.T) {
	withTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	agents := NewClient(2*time.Second).UsageContext(ctx, Keys{"Main": "secret"})
	if len(agents) != 1 || !strings.Contains(agents[0].Error, "context canceled") {
		t.Fatalf("agents = %+v, want cancellation error", agents)
	}
}

func TestUsageSharesOneCycleTimestampAcrossWorkers(t *testing.T) {
	payload := apiPayload()
	withTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer slow" {
			time.Sleep(40 * time.Millisecond)
		}
		fmt.Fprint(w, payload)
	})

	agents := NewClient(time.Second).Usage(Keys{"Fast": "fast", "Slow": "slow"})
	if len(agents) != 2 || agents[0].Error != "" || agents[1].Error != "" {
		t.Fatalf("agents = %+v", agents)
	}
	if agents[0].MonthlyDaysLeft() != agents[1].MonthlyDaysLeft() ||
		agents[0].WeeklyDryDays() != agents[1].WeeklyDryDays() {
		t.Fatalf("budgets differ despite identical payload: %+v", agents)
	}
}

func TestParseWindowsIgnoresMalformedOptionalRollingWindow(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	monthly, weekly := validRawWindows(now)
	rolling := &windowRaw{
		Status: "error", Percent: intPointer(101), ResetsAt: "not-a-date",
	}

	windows, err := parseWindowsAt(marshalUsage(t, monthly, weekly, rolling), now)
	if err != nil {
		t.Fatalf("parseWindowsAt rejected valid policy windows because rolling was malformed: %v", err)
	}
	if len(windows) != 2 || windows[0].Name != "Monthly" || windows[1].Name != "Weekly" {
		t.Fatalf("windows = %+v, want monthly and weekly only", windows)
	}
}
