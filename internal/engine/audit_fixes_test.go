package engine

import (
	"testing"

	"github.com/rjullien/bifrost-weight-sidecar/internal/bifrost"
)

func TestComputeHandlesMultipleUnhealthyKeysWithoutQuotaAgents(t *testing.T) {
	keys := []bifrost.Key{
		key("k1", "dead-1", "env.OPENCODE_GO_API_KEY_X", 1, "error"),
		key("k2", "dead-2", "env.OPENCODE_GO_API_KEY_Y", 1, "error"),
	}

	changes := Compute(Config{}, Input{Keys: keys})
	if len(changes) != 2 {
		t.Fatalf("changes = %d, want both unhealthy keys disabled", len(changes))
	}
	for _, change := range changes {
		if change.To != 0 {
			t.Fatalf("change = %+v, want weight 0", change)
		}
	}
}

func TestComputeTreatsMissingBifrostStatusAsUnhealthy(t *testing.T) {
	keys := []bifrost.Key{
		key("k1", "unknown", "env.OPENCODE_GO_API_KEY", 1, ""),
	}

	changes := Compute(Config{MinActive: 1}, Input{Keys: keys, Agents: healthyAgents()[:1]})
	if len(changes) != 1 || changes[0].To != 0 {
		t.Fatalf("changes = %+v, want unknown-health key disabled", changes)
	}
}

func TestComputeHonorsMinActiveAboveTwo(t *testing.T) {
	agents := healthyAgents()
	for i := range agents {
		agents[i] = agent(agents[i].Label, 40, 0, 20, 95, 2)
	}

	changes := Compute(Config{MinActive: 3}, Input{Keys: healthyKeys(), Agents: agents})
	final := map[string]float64{}
	for _, k := range healthyKeys() {
		final[k.Name] = k.Weight
	}
	for _, change := range changes {
		final[change.Key.Name] = change.To
	}
	active := 0
	for _, weight := range final {
		if weight > 0 {
			active++
		}
	}
	if active != 3 {
		t.Fatalf("active = %d, weights = %v, want 3", active, final)
	}
}

func TestLabelFromEnvRejectsMalformedReferences(t *testing.T) {
	for _, ref := range []string{
		"env.OPENCODE_GO_API_KEYA",
		"env.OPENCODE_GO_API_KEY_",
	} {
		if got := LabelFromEnv(ref); got != "" {
			t.Errorf("LabelFromEnv(%q) = %q, want empty", ref, got)
		}
	}
}
