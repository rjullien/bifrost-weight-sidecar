package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rjullien/bifrost-weight-sidecar/internal/bifrost"
	"github.com/rjullien/bifrost-weight-sidecar/internal/engine"
	"github.com/rjullien/bifrost-weight-sidecar/internal/quotas"
)

func TestLoadConfigRejectsInvalidInterval(t *testing.T) {
	for _, value := range []string{"0s", "-1s", "not-a-duration"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("INTERVAL", value)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("loadConfig accepted INTERVAL=%q", value)
			}
		})
	}
}

func TestLoadConfigAcceptsPositiveInterval(t *testing.T) {
	t.Setenv("INTERVAL", "45s")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Interval != 45*time.Second {
		t.Fatalf("Interval = %s, want 45s", cfg.Interval)
	}
}

func TestOpenCodeKeysFromEnvTrimsSecrets(t *testing.T) {
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OPENCODE_GO_API_KEY") {
			t.Setenv(name, "")
		}
	}
	t.Setenv("OPENCODE_GO_API_KEY", "  main-secret\n")
	t.Setenv("OPENCODE_GO_API_KEY_A", "\ta-secret ")
	t.Setenv("OPENCODE_GO_API_KEY_EMPTY", " \n")
	t.Setenv("OPENCODE_GO_API_KEY_", "must-not-create-an-empty-label")

	keys := openCodeKeysFromEnv()
	if keys["Main"] != "main-secret" || keys["A"] != "a-secret" {
		t.Fatalf("keys = %#v, want trimmed Main and A", keys)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %#v, want only Main and A", keys)
	}
}

func TestURLForLogRemovesCredentials(t *testing.T) {
	got := urlForLog("https://admin:secret@example.test:8443/api")
	if strings.Contains(got, "admin") || strings.Contains(got, "secret") {
		t.Fatalf("urlForLog leaked credentials: %q", got)
	}
	if got != "https://example.test:8443/api" {
		t.Fatalf("urlForLog = %q", got)
	}
}

func TestLoadConfigRejectsUnsafeBifrostURL(t *testing.T) {
	for _, value := range []string{
		"admin:secret@example.test:8443/api",
		"ftp://example.test/api",
		"https://example.test/api?token=topsecret",
		"https://example.test/api#secret",
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("BIFROST_URL", value)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("loadConfig accepted BIFROST_URL=%q", value)
			} else if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "token") {
				t.Fatalf("configuration error leaked credentials: %v", err)
			}
		})
	}
}

func TestURLForLogDropsQueryAndRejectsOpaqueValues(t *testing.T) {
	if got := urlForLog("https://example.test/api?token=topsecret#fragment"); got != "https://example.test/api" {
		t.Fatalf("urlForLog = %q, want URL without query or fragment", got)
	}
	if got := urlForLog("admin:secret@example.test:8443/api"); got != "<URL invalide>" {
		t.Fatalf("urlForLog opaque value = %q", got)
	}
}

func TestLoadConfigRejectsInvalidDryRun(t *testing.T) {
	t.Setenv("DRY_RUN", "treu")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "DRY_RUN") {
		t.Fatalf("loadConfig error = %v, want strict DRY_RUN rejection", err)
	}
}

func TestLoadConfigValidatesCycleTimeout(t *testing.T) {
	for _, value := range []string{"0s", "-1s", "invalid"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("CYCLE_TIMEOUT", value)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("loadConfig accepted CYCLE_TIMEOUT=%q", value)
			}
		})
	}

	t.Setenv("CYCLE_TIMEOUT", "42s")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.CycleTimeout != 42*time.Second {
		t.Fatalf("CycleTimeout = %s, want 42s", cfg.CycleTimeout)
	}
}

func TestLoadConfigValidatesRetryBackoff(t *testing.T) {
	for _, value := range []string{"0s", "-1s", "invalid"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("RETRY_BACKOFF", value)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("loadConfig accepted RETRY_BACKOFF=%q", value)
			}
		})
	}

	if cfg, err := loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	} else if cfg.RetryBackoff != 15*time.Second {
		t.Fatalf("RetryBackoff = %s, want default 15s", cfg.RetryBackoff)
	}

	t.Setenv("RETRY_BACKOFF", "20s")
	if cfg, err := loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	} else if cfg.RetryBackoff != 20*time.Second {
		t.Fatalf("RetryBackoff = %s, want 20s", cfg.RetryBackoff)
	}
}

// runCycle must signal that Bifrost is unreachable so the caller retries with a
// backoff instead of sleeping the full interval.
func TestRunCycleReportsBifrostUnreachable(t *testing.T) {
	bf := &fakeBifrost{keysErr: errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")}
	qu := &fakeQuotas{}

	if reachable := runCycle(context.Background(), config{}, engine.Config{MinActive: 1}, bf, qu); reachable {
		t.Fatal("runCycle reported reachable despite a Bifrost connection error")
	}
	if qu.calls != 0 {
		t.Fatalf("quota calls = %d, want 0 (short-circuit before quota fetch)", qu.calls)
	}
	if len(bf.setCalls) != 0 {
		t.Fatalf("setCalls = %v, want none when Bifrost is unreachable", bf.setCalls)
	}
}

type fakeBifrost struct {
	keys         []bifrost.Key
	keySnapshots map[string]bifrost.Key
	setErrors    map[string]error
	setCalls     []string
	keysErr      error
	persist      bool
}

func (f *fakeBifrost) KeysContext(context.Context) ([]bifrost.Key, error) {
	if f.keysErr != nil {
		return nil, f.keysErr
	}
	return append([]bifrost.Key(nil), f.keys...), nil
}

func (f *fakeBifrost) KeyContext(_ context.Context, id string) (bifrost.Key, error) {
	if key, ok := f.keySnapshots[id]; ok {
		return key, nil
	}
	for _, key := range f.keys {
		if key.ID == id {
			return key, nil
		}
	}
	return bifrost.Key{}, errors.New("key not found")
}

func (f *fakeBifrost) SetWeightContext(_ context.Context, key bifrost.Key, weight float64) error {
	f.setCalls = append(f.setCalls, key.ID)
	if err := f.setErrors[key.ID]; err != nil {
		return err
	}
	if f.persist {
		for i := range f.keys {
			if f.keys[i].ID == key.ID {
				f.keys[i].Weight = weight
			}
		}
	}
	return nil
}

type fakeQuotas struct {
	agents []quotas.Agent
	calls  int
}

func (f *fakeQuotas) UsageContext(context.Context, quotas.Keys) []quotas.Agent {
	f.calls++
	return f.agents
}

func testKey(id string, weight float64, status string) bifrost.Key {
	return bifrost.Key{
		ID: id, Name: id, Weight: weight, Status: status,
		Value: bifrost.SecretRef{Ref: "env.OPENCODE_GO_API_KEY_" + strings.ToUpper(id), Type: "env"},
	}
}

func TestRunCycleAppliesBifrostHealthWithoutQuotaAgents(t *testing.T) {
	key := testKey("a", 1, "error")
	bf := &fakeBifrost{keys: []bifrost.Key{key}, persist: true}
	qu := &fakeQuotas{}

	if reachable := runCycle(context.Background(), config{}, engine.Config{MinActive: 1}, bf, qu); !reachable {
		t.Fatal("runCycle reported Bifrost unreachable despite a successful snapshot")
	}
	if qu.calls != 1 {
		t.Fatalf("quota calls = %d, want 1", qu.calls)
	}
	if len(bf.setCalls) != 1 || bf.setCalls[0] != "a" || bf.keys[0].Weight != 0 {
		t.Fatalf("setCalls=%v keys=%+v, want unhealthy key forced to zero", bf.setCalls, bf.keys)
	}
}

func TestApplyChangesRunsIncreasesBeforeDecreases(t *testing.T) {
	increase := testKey("increase", 0, "success")
	decrease := testKey("decrease", 1, "success")
	bf := &fakeBifrost{keys: []bifrost.Key{increase, decrease}, persist: true}
	changes := []engine.Change{
		{Key: decrease, From: 1, To: 0},
		{Key: increase, From: 0, To: 1},
	}

	result, err := applyChanges(context.Background(), bf, changes)
	if err != nil {
		t.Fatalf("applyChanges: %v", err)
	}
	if got := strings.Join(bf.setCalls, ","); got != "increase,decrease" {
		t.Fatalf("set order = %q, want increase,decrease", got)
	}
	if result.Succeeded != 2 || result.Failed != 0 || result.Skipped != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestApplyChangesSkipsDecreasesAfterFailedActivation(t *testing.T) {
	increase := testKey("increase", 0, "success")
	decrease := testKey("decrease", 1, "success")
	bf := &fakeBifrost{
		keys:      []bifrost.Key{increase, decrease},
		setErrors: map[string]error{"increase": errors.New("write failed")},
		persist:   true,
	}
	changes := []engine.Change{
		{Key: decrease, From: 1, To: 0},
		{Key: increase, From: 0, To: 1},
	}

	result, err := applyChanges(context.Background(), bf, changes)
	if err != nil {
		t.Fatalf("applyChanges unexpected final-verification error: %v", err)
	}
	if got := strings.Join(bf.setCalls, ","); got != "increase" {
		t.Fatalf("set calls = %q, want only failed increase", got)
	}
	if result.Failed != 1 || result.Skipped != 1 || result.Succeeded != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestApplyChangesSkipsConcurrentWeightAndDependentDecrease(t *testing.T) {
	increase := testKey("increase", 0, "success")
	concurrent := increase
	concurrent.Weight = 0.25
	decrease := testKey("decrease", 1, "success")
	bf := &fakeBifrost{
		keys:         []bifrost.Key{increase, decrease},
		keySnapshots: map[string]bifrost.Key{"increase": concurrent},
		persist:      true,
	}
	changes := []engine.Change{
		{Key: decrease, From: 1, To: 0},
		{Key: increase, From: 0, To: 1},
	}

	result, err := applyChanges(context.Background(), bf, changes)
	if err != nil {
		t.Fatalf("applyChanges: %v", err)
	}
	if len(bf.setCalls) != 0 || result.Skipped != 2 || result.Failed != 0 {
		t.Fatalf("calls=%v result=%+v, want both changes skipped", bf.setCalls, result)
	}
}

func TestApplyChangesReportsFinalStateMismatch(t *testing.T) {
	increase := testKey("increase", 0, "success")
	bf := &fakeBifrost{keys: []bifrost.Key{increase}, persist: false}
	result, err := applyChanges(context.Background(), bf, []engine.Change{{
		Key: increase, From: 0, To: 1,
	}})
	if err == nil || !strings.Contains(err.Error(), "différent") {
		t.Fatalf("applyChanges error = %v, want final-state mismatch", err)
	}
	if result.Succeeded != 1 || result.Failed != 1 {
		t.Fatalf("result = %+v, want one acknowledged write and one verification failure", result)
	}
}

func TestApplyChangesSkipsDecisionInputDrift(t *testing.T) {
	tests := []struct {
		name    string
		planned bifrost.Key
		current bifrost.Key
		from    float64
		to      float64
	}{
		{
			name:    "activation became unhealthy",
			planned: testKey("increase", 0, "success"),
			current: testKey("increase", 0, "error"),
			from:    0, to: 1,
		},
		{
			name:    "deactivation recovered",
			planned: testKey("decrease", 1, "error"),
			current: testKey("decrease", 1, "success"),
			from:    1, to: 0,
		},
		{
			name:    "subscription reference changed",
			planned: testKey("increase", 0, "success"),
			current: func() bifrost.Key {
				key := testKey("increase", 0, "success")
				key.Value.Ref = "env.OPENCODE_GO_API_KEY_OTHER"
				return key
			}(),
			from: 0, to: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bf := &fakeBifrost{
				keys:         []bifrost.Key{test.planned},
				keySnapshots: map[string]bifrost.Key{test.planned.ID: test.current},
				persist:      true,
			}
			result, err := applyChanges(context.Background(), bf, []engine.Change{{
				Key: test.planned, From: test.from, To: test.to,
			}})
			if err != nil {
				t.Fatalf("applyChanges: %v", err)
			}
			if len(bf.setCalls) != 0 || result.Skipped != 1 || result.Failed != 0 {
				t.Fatalf("calls=%v result=%+v, want stale decision skipped", bf.setCalls, result)
			}
		})
	}
}

type finalSnapshotBifrost struct {
	*fakeBifrost
	final []bifrost.Key
}

func (f *finalSnapshotBifrost) KeysContext(context.Context) ([]bifrost.Key, error) {
	return append([]bifrost.Key(nil), f.final...), nil
}

func TestApplyChangesReportsFinalDecisionInputDrift(t *testing.T) {
	increase := testKey("increase", 0, "success")
	final := increase
	final.Weight = 1
	final.Status = "error"
	bf := &finalSnapshotBifrost{
		fakeBifrost: &fakeBifrost{keys: []bifrost.Key{increase}, persist: true},
		final:       []bifrost.Key{final},
	}
	result, err := applyChanges(context.Background(), bf, []engine.Change{{
		Key: increase, From: 0, To: 1,
	}})
	if err == nil || !strings.Contains(err.Error(), "différent") {
		t.Fatalf("applyChanges error = %v, want final decision-input mismatch", err)
	}
	if result.Succeeded != 1 || result.Failed != 1 {
		t.Fatalf("result = %+v, want one write and one verification failure", result)
	}
}
