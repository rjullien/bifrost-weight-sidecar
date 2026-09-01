package engine

import (
	"testing"

	"github.com/rjullien/bifrost-weight-sidecar/internal/bifrost"
	"github.com/rjullien/bifrost-weight-sidecar/internal/quotas"
)

func key(id, name, ref string, weight float64, status string) bifrost.Key {
	return bifrost.Key{ID: id, Name: name, Value: bifrost.SecretRef{Ref: ref, Type: "env"}, Weight: weight, Status: status}
}

// agent builds a quota agent: monthly percent + monthly dry days + monthly
// days left + weekly percent + weekly dry days.
func agent(label string, monthlyPct int, monthlyDry, monthlyDaysLeft float64, weeklyPct int, weeklyDry float64) quotas.Agent {
	return quotas.Agent{
		Label: label,
		Windows: []quotas.Window{
			{Name: "Monthly", Percent: monthlyPct,
				Budget: &quotas.Budget{Valid: true, DryDays: monthlyDry, DaysLeft: monthlyDaysLeft}},
			{Name: "Weekly", Percent: weeklyPct,
				Budget: &quotas.Budget{Valid: true, DryDays: weeklyDry}},
			{Name: "Rolling 5h", Percent: 0},
		},
	}
}

// healthyAgents: all keys healthy, monthly mid-cycle with plenty of days left.
// (80% consumed, 20 days left → urgency 20/20 = 1, matching the initial
// weight 1 so healthy keys produce no diff.)
func healthyAgents() []quotas.Agent {
	return []quotas.Agent{
		agent("Main", 80, 0, 20, 50, 0),
		agent("R", 80, 0, 20, 50, 0),
		agent("A", 80, 0, 20, 50, 0),
		agent("N", 80, 0, 20, 50, 0),
	}
}

func healthyKeys() []bifrost.Key {
	return []bifrost.Key{
		key("k1", "opencode-go-key-1", "env.OPENCODE_GO_API_KEY", 1, "success"),
		key("k2", "opencode-go-key-2", "env.OPENCODE_GO_API_KEY_R", 1, "success"),
		key("k3", "opencode-go-key-3", "env.OPENCODE_GO_API_KEY_A", 1, "success"),
		key("k4", "opencode-go-key-4", "env.OPENCODE_GO_API_KEY_N", 1, "success"),
	}
}

func TestComputeKeepsSameUrgencyKeysInRotation(t *testing.T) {
	cfg := Config{}
	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: healthyAgents()})
	// All keys identical urgency (60% remaining / 20 days = 3) and already at
	// weight 1 → no changes.
	if len(changes) != 0 {
		t.Errorf("changes = %d, want 0 (identical urgency, no diff)", len(changes))
	}
}

// A key about to lose monthly quota (5% left, reset tomorrow) must receive a
// much higher weight than a key with 20 days left.
func TestComputePushesKeyWithExpiringQuota(t *testing.T) {
	cfg := Config{}
	agents := healthyAgents()
	agents[0] = agent("Main", 95, 0, 1, 50, 0) // 5% left, resets in 1 day

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	var mainTo, nTo float64
	for _, c := range changes {
		if c.Key.Name == "opencode-go-key-1" {
			mainTo = c.To
		}
		if c.Key.Name == "opencode-go-key-4" {
			nTo = c.To
		}
	}
	if mainTo != 5 {
		t.Errorf("Main to = %v, want 5 (5%% left / 1 day)", mainTo)
	}
	if nTo != 0 {
		t.Errorf("N to = %v, want 0 (urgency unchanged 3 == current 1)", nTo)
	}
}

// Monthly quota exhausted → weight 0 (nothing left to burn).
func TestComputeZerosKeyWhenMonthlyDry(t *testing.T) {
	cfg := Config{}
	agents := healthyAgents()
	agents[1] = agent("R", 100, 5, 5, 50, 0) // R at the ceiling for 5 days

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	if changes[0].Key.Name != "opencode-go-key-2" || changes[0].To != 0 {
		t.Errorf("change = %+v, want key-2 -> 0", changes[0])
	}
}

// Weekly projected dry → anticipate the block: 0 even with monthly quota left.
func TestComputeZerosKeyWhenWeeklyBlocks(t *testing.T) {
	cfg := Config{}
	agents := healthyAgents()
	agents[2] = agent("A", 40, 0, 20, 95, 1.5) // weekly hits ceiling in 1.5 days

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	if changes[0].Key.Name != "opencode-go-key-3" || changes[0].To != 0 {
		t.Errorf("change = %+v, want key-3 -> 0 (weekly blocker)", changes[0])
	}
}

// Bifrost reporting a key as not healthy is a hard kill, even without any
// quota signal for that key.
func TestComputeZerosKeyWhenBifrostReportsUnhealthy(t *testing.T) {
	cfg := Config{}
	keys := healthyKeys()
	keys[2].Status = "error"
	agents := healthyAgents()[:0] // no quota data at all

	changes := Compute(cfg, Input{Keys: keys, Agents: agents})
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	if changes[0].Key.Name != "opencode-go-key-3" || changes[0].To != 0 {
		t.Errorf("change = %+v, want key-3 -> 0", changes[0])
	}
}

// A key whose quotas cannot be assessed must be left alone: zeroing it on
// incomplete data would break a healthy subscription on a dashboard blip.
func TestComputeLeavesKeyAloneWhenQuotasUnknown(t *testing.T) {
	cfg := Config{}

	// Key healthy, but its label is absent from the quota payload.
	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: healthyAgents()[:3]})
	if len(changes) != 0 {
		t.Errorf("changes = %d, want 0 (key-4 label missing)", len(changes))
	}

	// Key healthy, but its agent is in error.
	errAgents := healthyAgents()
	errAgents[0].Error = "clé invalide ou expirée (HTTP 401)"
	changes = Compute(cfg, Input{Keys: healthyKeys(), Agents: errAgents})
	if len(changes) != 0 {
		t.Errorf("changes = %d, want 0 (Main fetch failed)", len(changes))
	}
}

func TestComputeIgnoresPinnedKeys(t *testing.T) {
	cfg := Config{Pinned: map[string]bool{"opencode-go-key-1": true}}
	agents := healthyAgents()
	agents[0] = agent("Main", 100, 5, 5, 50, 0) // would normally be zeroed

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want 0 (key-1 pinned)", changes)
	}
}

func TestComputePinsByIdToo(t *testing.T) {
	cfg := Config{Pinned: map[string]bool{"k1": true}}
	agents := healthyAgents()
	agents[0] = agent("Main", 100, 5, 5, 50, 0)

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want 0 (key-1 pinned by id)", changes)
	}
}

// Even when every key is projected dry (weekly or monthly), MinActive keys
// must keep a non-zero weight: the pool never loses its spare. A key blocked
// by the weekly (but with burnable monthly quota left) is re-armed as the
// fallback.
func TestComputeKeepsAtLeastMinActiveKeysAsFallback(t *testing.T) {
	cfg := Config{}
	agents := healthyAgents()
	// Main blocked by weekly (monthly still has quota to burn → re-armed),
	// R and A at the monthly ceiling (nothing left → stay 0), N healthy.
	agents[0] = agent("Main", 95, 0, 1, 95, 2) // urgency 5/1 = 5 → re-armed to 5
	agents[1] = agent("R", 100, 4, 4, 50, 0)
	agents[2] = agent("A", 100, 4, 4, 50, 0)

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	to := map[string]float64{}
	for _, c := range changes {
		to[c.Key.Name] = c.To
	}
	if to["opencode-go-key-1"] != 5 {
		t.Errorf("Main to = %v, want 5 (fallback: burnable monthly, re-armed to its urgency)", to["opencode-go-key-1"])
	}
	if to["opencode-go-key-2"] != 0 || to["opencode-go-key-3"] != 0 {
		t.Errorf("R/A to = %v/%v, want 0 (monthly ceiling, nothing to burn)", to["opencode-go-key-2"], to["opencode-go-key-3"])
	}
	// N healthy → keeps urgency 1 (no change needed, already 1).
	if _, ok := to["opencode-go-key-4"]; ok {
		t.Errorf("N changed to %v, want no change (already at urgency 1)", to["opencode-go-key-4"])
	}
}

func TestLabelFromEnv(t *testing.T) {
	cases := map[string]string{
		"env.OPENCODE_GO_API_KEY":       "Main",
		"env.OPENCODE_GO_API_KEY_R":     "R",
		"env.OPENCODE_GO_API_KEY_A":     "A",
		"env.OPENCODE_GO_API_KEY_ALICE": "ALICE",
		"env.OPENCODE_GO_API_KEY_N":     "N",
		"env.SOMETHING_ELSE":            "",
		"OPENCODE_GO_API_KEY":           "",
	}
	for ref, want := range cases {
		if got := LabelFromEnv(ref); got != want {
			t.Errorf("LabelFromEnv(%q) = %q, want %q", ref, got, want)
		}
	}
}
