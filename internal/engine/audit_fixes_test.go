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

// With MinActive=3 and only two keys able to serve (the other two burned at
// 100%), the burn pool allocates to the two live under-burners and never
// resurrects the burned ones. Active ends at 2, not 3.
func TestComputeHonorsMinActiveAboveTwo(t *testing.T) {
	agents := healthyAgents()
	agents[0] = agent("Main", 100, 4, 4, 50, 0) // burned
	agents[1] = agent("R", 100, 4, 4, 50, 0)    // burned
	agents[2] = agent("A", 40, 0, 20, 50, 0)    // under-burner
	agents[3] = agent("N", 40, 0, 20, 50, 0)    // under-burner

	changes := Compute(Config{MinActive: 3}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)
	if final["opencode-go-key-1"] != 0 || final["opencode-go-key-2"] != 0 {
		t.Errorf("burned keys alive! Main=%v R=%v, want 0/0", final["opencode-go-key-1"], final["opencode-go-key-2"])
	}
	if !WeightsEqual(final["opencode-go-key-3"], 50) || !WeightsEqual(final["opencode-go-key-4"], 50) {
		t.Fatalf("weights = %v, want A=N=50 (equal under-burners)", final)
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
