package engine

import (
	"testing"

	"github.com/rjullien/bifrost-weight-sidecar/internal/bifrost"
	"github.com/rjullien/bifrost-weight-sidecar/internal/quotas"
)

func key(id, name, ref string, weight float64, status string) bifrost.Key {
	return bifrost.Key{ID: id, Name: name, Value: bifrost.SecretRef{Ref: ref, Type: "env"}, Weight: weight, Status: status}
}

func agent(label string, weekly int, dryDays float64) quotas.Agent {
	return quotas.Agent{
		Label: label,
		Windows: []quotas.Window{
			{Name: "Monthly", Percent: 40, Budget: &quotas.Budget{Valid: true, DryDays: dryDays}},
			{Name: "Weekly", Percent: weekly},
		},
	}
}

// healthyKeysFn builds a fresh key set per test: several tests mutate a key
// (weight, status) and must never leak that mutation into the others.
func healthyKeysFn() []bifrost.Key {
	return []bifrost.Key{
		key("k1", "opencode-go-key-1", "env.OPENCODE_GO_API_KEY", 1, "success"),
		key("k2", "opencode-go-key-2", "env.OPENCODE_GO_API_KEY_R", 1, "success"),
		key("k3", "opencode-go-key-3", "env.OPENCODE_GO_API_KEY_A", 1, "success"),
		key("k4", "opencode-go-key-4", "env.OPENCODE_GO_API_KEY_N", 1, "success"),
	}
}

// healthyAgentsFn builds a fresh agent set per test (same aliasing concern as
// healthyKeysFn: several tests replace one agent's quotas).
func healthyAgentsFn() []quotas.Agent {
	return []quotas.Agent{
		agent("Main", 50, 0),
		agent("R", 10, 0),
		agent("A", 10, 0),
		agent("N", 10, 0),
	}
}

func TestComputeKeepsHealthyKeysInRotation(t *testing.T) {
	cfg := Config{WeeklyThreshold: 80}
	changes := Compute(cfg, Input{Keys: healthyKeysFn(), Agents: healthyAgentsFn()})
	if len(changes) != 0 {
		t.Errorf("changes = %d, want 0 (all keys healthy, weights already 1)", len(changes))
	}
}

// A key taken out of rotation comes back as soon as its quotas are healthy:
// the controller is dynamic, not a one-way switch.
func TestComputeReenablesZeroedKey(t *testing.T) {
	cfg := Config{WeeklyThreshold: 80}
	keys := healthyKeysFn()
	keys[0].Weight = 0 // Main was manually zeroed, quotas are fine now

	changes := Compute(cfg, Input{Keys: keys, Agents: healthyAgentsFn()})
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	if changes[0].Key.Name != "opencode-go-key-1" || changes[0].To != 1 {
		t.Errorf("change = %+v, want key-1 -> 1", changes[0])
	}
}

func TestComputeZerosKeyWhenWeeklyThresholdReached(t *testing.T) {
	cfg := Config{WeeklyThreshold: 80}
	agents := healthyAgentsFn()
	agents[0] = agent("Main", 87, 0) // Main weekly at 87%

	changes := Compute(cfg, Input{Keys: healthyKeysFn(), Agents: agents})
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	if changes[0].Key.Name != "opencode-go-key-1" || changes[0].To != 0 {
		t.Errorf("change = %+v, want key-1 -> 0", changes[0])
	}
}

func TestComputeZerosKeyWhenMonthlyQuotaDry(t *testing.T) {
	cfg := Config{WeeklyThreshold: 80}
	agents := healthyAgentsFn()
	agents[1] = agent("R", 10, 3.5) // R will sit at the ceiling for 3.5 days

	changes := Compute(cfg, Input{Keys: healthyKeysFn(), Agents: agents})
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	if changes[0].Key.Name != "opencode-go-key-2" || changes[0].To != 0 {
		t.Errorf("change = %+v, want key-2 -> 0", changes[0])
	}
}

// Bifrost reporting a key as not healthy is a hard kill, even without any
// dashboard quota signal for that key.
func TestComputeZerosKeyWhenBifrostReportsUnhealthy(t *testing.T) {
	cfg := Config{WeeklyThreshold: 80}
	keys := healthyKeysFn()
	keys[2].Status = "error"
	agents := healthyAgentsFn()[:0] // no dashboard data at all

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
	cfg := Config{WeeklyThreshold: 80}

	// Key healthy, but its label is absent from the dashboard payload.
	changes := Compute(cfg, Input{Keys: healthyKeysFn(), Agents: healthyAgentsFn()[:3]})
	if len(changes) != 0 {
		t.Errorf("changes = %d, want 0 (key-4 label missing)", len(changes))
	}

	// Key healthy, but its dashboard agent is in error.
	errAgents := healthyAgentsFn()
	errAgents[0].Error = "clé invalide ou expirée (HTTP 401)"
	changes = Compute(cfg, Input{Keys: healthyKeysFn(), Agents: errAgents})
	if len(changes) != 0 {
		t.Errorf("changes = %d, want 0 (Main dashboard fetch failed)", len(changes))
	}
}

func TestComputeIgnoresPinnedKeys(t *testing.T) {
	cfg := Config{WeeklyThreshold: 80, Pinned: map[string]bool{"opencode-go-key-1": true}}
	agents := healthyAgentsFn()
	agents[0] = agent("Main", 87, 0) // would normally be zeroed

	changes := Compute(cfg, Input{Keys: healthyKeysFn(), Agents: agents})
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want 0 (key-1 pinned)", changes)
	}
}

func TestComputePinsByIdToo(t *testing.T) {
	cfg := Config{WeeklyThreshold: 80, Pinned: map[string]bool{"k1": true}}
	agents := healthyAgentsFn()
	agents[0] = agent("Main", 87, 0)

	changes := Compute(cfg, Input{Keys: healthyKeysFn(), Agents: agents})
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want 0 (key-1 pinned by id)", changes)
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
