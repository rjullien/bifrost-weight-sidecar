package quotas

import (
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
		fmt.Fprint(w, apiPayload)
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
		fmt.Fprint(w, apiPayload)
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
