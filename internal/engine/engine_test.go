package engine

import (
	"math"
	"testing"

	"github.com/rjullien/bifrost-weight-sidecar/internal/bifrost"
	"github.com/rjullien/bifrost-weight-sidecar/internal/quotas"
)

func key(id, name, ref string, weight float64, status string) bifrost.Key {
	return bifrost.Key{ID: id, Name: name, Value: bifrost.SecretRef{Ref: ref, Type: "env"}, Weight: weight, Status: status}
}

// agent builds a quota agent: monthly percent + monthly dry days + monthly
// days left + weekly percent + weekly dry days. Rolling 5h defaults to 0.
//
// MonthlyDryDays semantics (from quotas.computeBudget):
//   - DryDays > 0 → projected to hit 100% before reset (on track)
//   - DryDays == 0 → will NOT hit 100% at current pace (under-burner)
func agent(label string, monthlyPct int, monthlyDry, monthlyDaysLeft float64, weeklyPct int, weeklyDry float64) quotas.Agent {
	return agentRolling(label, monthlyPct, monthlyDry, monthlyDaysLeft, weeklyPct, weeklyDry, 0)
}

func agentRolling(label string, monthlyPct int, monthlyDry, monthlyDaysLeft float64, weeklyPct int, weeklyDry float64, rollingPct int) quotas.Agent {
	return quotas.Agent{
		Label: label,
		Windows: []quotas.Window{
			{Name: "Monthly", Percent: monthlyPct,
				Budget: &quotas.Budget{Valid: true, DryDays: monthlyDry, DaysLeft: monthlyDaysLeft}},
			{Name: "Weekly", Percent: weeklyPct,
				Budget: &quotas.Budget{Valid: true, DryDays: weeklyDry}},
			{Name: "Rolling 5h", Percent: rollingPct},
		},
	}
}

// healthyAgents: all on track (DryDays > 0), equal urgency 1 (20%/20d).
// Equal share of 100 → 25 each when no under-burner steals the pool.
func healthyAgents() []quotas.Agent {
	return []quotas.Agent{
		agent("Main", 80, 2, 20, 50, 0),
		agent("R", 80, 2, 20, 50, 0),
		agent("A", 80, 2, 20, 50, 0),
		agent("N", 80, 2, 20, 50, 0),
	}
}

func healthyKeys() []bifrost.Key {
	return []bifrost.Key{
		key("k1", "opencode-go-key-1", "env.OPENCODE_GO_API_KEY", 25, "success"),
		key("k2", "opencode-go-key-2", "env.OPENCODE_GO_API_KEY_R", 25, "success"),
		key("k3", "opencode-go-key-3", "env.OPENCODE_GO_API_KEY_A", 25, "success"),
		key("k4", "opencode-go-key-4", "env.OPENCODE_GO_API_KEY_N", 25, "success"),
	}
}

func finalWeights(keys []bifrost.Key, changes []Change) map[string]float64 {
	final := map[string]float64{}
	for _, k := range keys {
		final[k.Name] = k.Weight
		final[k.ID] = k.Weight
	}
	for _, c := range changes {
		final[c.Key.Name] = c.To
		final[c.Key.ID] = c.To
	}
	return final
}

func sumPositive(weights map[string]float64, names ...string) float64 {
	var sum float64
	for _, name := range names {
		if w := weights[name]; w > 0 {
			sum += w
		}
	}
	return sum
}

func TestComputeKeepsSameUrgencyKeysInRotation(t *testing.T) {
	changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: healthyAgents()})
	// All on-track, equal urgency → 25 each, already at 25 → no changes.
	if len(changes) != 0 {
		t.Errorf("changes = %d, want 0 (identical urgency, already at 25%%)", len(changes))
	}
}

// A single under-burner receives 100%; on-track keys get 0.
func TestComputeUnderBurnerGetsHundred(t *testing.T) {
	agents := healthyAgents()
	// N: under-burner (DryDays=0), 8% left / 1.4d — prod-like shortfall.
	agents[3] = agent("N", 92, 0, 1.4, 50, 0)
	// Main/R/A stay on track (DryDays > 0).

	changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)
	if final["opencode-go-key-4"] != 100 {
		t.Errorf("N = %v, want 100 (sole under-burner)", final["opencode-go-key-4"])
	}
	for _, name := range []string{"opencode-go-key-1", "opencode-go-key-2", "opencode-go-key-3"} {
		if final[name] != 0 {
			t.Errorf("%s = %v, want 0 (on-track while under-burner exists)", name, final[name])
		}
	}
	if sum := sumPositive(final, "opencode-go-key-1", "opencode-go-key-2", "opencode-go-key-3", "opencode-go-key-4"); !WeightsEqual(sum, 100) {
		t.Errorf("sum of applied targets = %v, want 100", sum)
	}
}

// Two under-burners split by urgency, normalized to 100.
func TestComputeTwoUnderBurnersSplitNormalizedToHundred(t *testing.T) {
	agents := healthyAgents()
	// N: 8%/1.4d ≈ 5.714; Main: 3%/1.5d = 2. Both DryDays=0.
	agents[0] = agent("Main", 97, 0, 1.5, 50, 0)
	agents[3] = agent("N", 92, 0, 1.4, 50, 0)
	// R/A on track.

	changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)

	if final["opencode-go-key-2"] != 0 || final["opencode-go-key-3"] != 0 {
		t.Errorf("on-track R/A must be 0, got R=%v A=%v", final["opencode-go-key-2"], final["opencode-go-key-3"])
	}
	main, n := final["opencode-go-key-1"], final["opencode-go-key-4"]
	if main <= 0 || n <= 0 {
		t.Fatalf("both under-burners need weight, Main=%v N=%v", main, n)
	}
	if n <= main {
		t.Errorf("N (%v) should outrank Main (%v) (higher urgency)", n, main)
	}
	if !WeightsEqual(main+n, 100) {
		t.Errorf("Main+N = %v, want 100", main+n)
	}
	// Rough expected share: urgencies 8/1.4 ≈ 5.714 vs 3/1.5 = 2 → ~74 / ~26.
	if n < 70 || n > 78 || main < 22 || main > 30 {
		t.Errorf("Main=%v N=%v, want roughly ~26 / ~74", main, n)
	}
}

// On-track key gets 0 when an under-burner exists (does not need traffic).
func TestComputeOnTrackGetsZeroWhenUnderBurnerExists(t *testing.T) {
	agents := healthyAgents()
	agents[1] = agent("R", 70, 3, 2, 50, 0) // on track, high raw urgency 15
	agents[3] = agent("N", 90, 0, 2, 50, 0) // under-burner, urgency 5

	changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)
	if final["opencode-go-key-2"] != 0 {
		t.Errorf("R on-track = %v, want 0 while under-burner exists", final["opencode-go-key-2"])
	}
	if final["opencode-go-key-4"] != 100 {
		t.Errorf("N under-burner = %v, want 100", final["opencode-go-key-4"])
	}
}

// Zero under-burners: fall back to urgency among remaining, normalized to 100.
func TestComputeFallbackUrgencyWhenAllOnTrack(t *testing.T) {
	agents := healthyAgents()
	agents[0] = agent("Main", 95, 1, 1, 50, 0) // on track, urgency 5
	// Others on track urgency 1.

	changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)
	// scores 5+1+1+1=8 → Main 62.5, others 12.5
	if !WeightsEqual(final["opencode-go-key-1"], 62.5) {
		t.Errorf("Main = %v, want 62.5", final["opencode-go-key-1"])
	}
	for _, name := range []string{"opencode-go-key-2", "opencode-go-key-3", "opencode-go-key-4"} {
		if !WeightsEqual(final[name], 12.5) {
			t.Errorf("%s = %v, want 12.5", name, final[name])
		}
	}
	if !WeightsEqual(sumPositive(final, "opencode-go-key-1", "opencode-go-key-2", "opencode-go-key-3", "opencode-go-key-4"), 100) {
		t.Errorf("sum = %v, want 100", sumPositive(final, "opencode-go-key-1", "opencode-go-key-2", "opencode-go-key-3", "opencode-go-key-4"))
	}
}

func TestComputeZerosKeyWhenMonthlyDry(t *testing.T) {
	agents := healthyAgents()
	agents[1] = agent("R", 100, 5, 5, 50, 0)

	changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)
	if final["opencode-go-key-2"] != 0 {
		t.Errorf("R = %v, want 0 (monthly 100%%)", final["opencode-go-key-2"])
	}
}

// Projected-dry monthly below 100% is on track: still eligible in the
// zero-under-burner fallback, never hard-evicted by projection alone.
func TestComputeKeepsMonthlyProjectedDryButBelowCeiling(t *testing.T) {
	agents := healthyAgents()
	agents[1] = agent("R", 70, 3, 2, 50, 0) // on track, urgency 15

	changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)
	if final["opencode-go-key-2"] == 0 {
		t.Error("R zeroed while below 100% with no under-burner — want share of fallback")
	}
	if final["opencode-go-key-2"] <= final["opencode-go-key-1"] {
		t.Errorf("R (%v) should outrank Main (%v)", final["opencode-go-key-2"], final["opencode-go-key-1"])
	}
	if !WeightsEqual(sumPositive(final, "opencode-go-key-1", "opencode-go-key-2", "opencode-go-key-3", "opencode-go-key-4"), 100) {
		t.Errorf("sum = %v, want 100", sumPositive(final, "opencode-go-key-1", "opencode-go-key-2", "opencode-go-key-3", "opencode-go-key-4"))
	}
}

func TestComputeZerosKeyWhenWeeklyBlocks(t *testing.T) {
	agents := healthyAgents()
	agents[2] = agent("A", 40, 0, 20, 99, 0) // under-burner but weekly blocked

	changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)
	if final["opencode-go-key-3"] != 0 {
		t.Errorf("A = %v, want 0 (weekly blocker)", final["opencode-go-key-3"])
	}
}

func TestComputeKeepsKeyWhenWeeklyProjectedDryButBelowThreshold(t *testing.T) {
	agents := healthyAgents()
	agents[2] = agent("A", 80, 2, 20, 80, 1.5) // on track monthly, weekly projection ignored

	changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents})
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want 0 (weekly 80%% < 99%%, projection ignored)", changes)
	}
}

func TestComputeWeeklyThresholdConfigurable(t *testing.T) {
	agents := healthyAgents()
	agents[2] = agent("A", 80, 2, 20, 92, 0)

	if changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents}); len(changes) != 0 {
		t.Errorf("default threshold: changes = %+v, want 0 (92%% < 99%%)", changes)
	}

	changes := Compute(Config{WeeklyEvictPercent: 90}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)
	if final["opencode-go-key-3"] != 0 {
		t.Errorf("threshold 90: A = %v, want 0", final["opencode-go-key-3"])
	}
}

func TestComputeZerosKeyWhenBifrostReportsUnhealthy(t *testing.T) {
	keys := healthyKeys()
	keys[2].Status = "error"
	agents := healthyAgents()[:0]

	changes := Compute(Config{}, Input{Keys: keys, Agents: agents})
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	if changes[0].Key.Name != "opencode-go-key-3" || changes[0].To != 0 {
		t.Errorf("change = %+v, want key-3 -> 0", changes[0])
	}
}

func TestComputeLeavesKeyAloneWhenQuotasUnknown(t *testing.T) {
	// Key-4 absent from quotas: leave it untouched. The other three on-track
	// keys are re-normalized among themselves (100/3 each).
	changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: healthyAgents()[:3]})
	for _, c := range changes {
		if c.Key.Name == "opencode-go-key-4" {
			t.Fatalf("missing-quota key changed: %+v", c)
		}
	}
	final := finalWeights(healthyKeys(), changes)
	if final["opencode-go-key-4"] != 25 {
		t.Errorf("key-4 = %v, want untouched 25", final["opencode-go-key-4"])
	}
	if !WeightsEqual(final["opencode-go-key-1"]+final["opencode-go-key-2"]+final["opencode-go-key-3"], 100) {
		t.Errorf("assessable sum = %v, want 100", final["opencode-go-key-1"]+final["opencode-go-key-2"]+final["opencode-go-key-3"])
	}

	// Main agent in error: leave Main untouched; re-normalize the others.
	errAgents := healthyAgents()
	errAgents[0].Error = "clé invalide ou expirée (HTTP 401)"
	changes = Compute(Config{}, Input{Keys: healthyKeys(), Agents: errAgents})
	for _, c := range changes {
		if c.Key.Name == "opencode-go-key-1" {
			t.Fatalf("error-agent key changed: %+v", c)
		}
	}
	final = finalWeights(healthyKeys(), changes)
	if final["opencode-go-key-1"] != 25 {
		t.Errorf("Main = %v, want untouched 25", final["opencode-go-key-1"])
	}
}

func TestComputeIgnoresPinnedKeys(t *testing.T) {
	agents := healthyAgents()
	agents[0] = agent("Main", 100, 5, 5, 50, 0)

	changes := Compute(Config{Pinned: map[string]bool{"opencode-go-key-1": true}}, Input{Keys: healthyKeys(), Agents: agents})
	for _, c := range changes {
		if c.Key.Name == "opencode-go-key-1" {
			t.Errorf("pinned key changed: %+v", c)
		}
	}
}

func TestComputePinsByIdToo(t *testing.T) {
	agents := healthyAgents()
	agents[0] = agent("Main", 100, 5, 5, 50, 0)

	changes := Compute(Config{Pinned: map[string]bool{"k1": true}}, Input{Keys: healthyKeys(), Agents: agents})
	for _, c := range changes {
		if c.Key.ID == "k1" {
			t.Errorf("pinned key changed: %+v", c)
		}
	}
}

// Single under-burner must keep 100%: MinActive must NOT dilute with a spare.
func TestComputeMinActiveDoesNotDiluteSingleUnderBurner(t *testing.T) {
	agents := healthyAgents()
	agents[3] = agent("N", 92, 0, 1.4, 50, 0) // sole under-burner

	changes := Compute(Config{MinActive: 2}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)
	if final["opencode-go-key-4"] != 100 {
		t.Errorf("N = %v, want 100 (MinActive must not dilute burn-to-100%%)", final["opencode-go-key-4"])
	}
	alive := 0
	for _, name := range []string{"opencode-go-key-1", "opencode-go-key-2", "opencode-go-key-3", "opencode-go-key-4"} {
		if final[name] > 0 {
			alive++
		}
	}
	if alive != 1 {
		t.Errorf("alive = %d, want 1 (winner-take-all under-burner)", alive)
	}
}

// Empty pool: re-arm eligible spares then re-normalize to 100.
func TestComputeFailOpenRearmsEmptyPoolNormalizedToHundred(t *testing.T) {
	keys := []bifrost.Key{
		key("main", "main", "env.OPENCODE_GO_API_KEY", 0, "success"),
		key("a", "a", "env.OPENCODE_GO_API_KEY_A", 0, "success"),
	}
	// Tiny urgencies round allocation to 0 → empty pool → fail-open.
	agents := []quotas.Agent{
		agent("Main", 99, 0, 10000, 50, 0),
		agent("A", 98, 0, 10000, 50, 0),
	}
	changes := Compute(Config{MinActive: 2}, Input{Keys: keys, Agents: agents})
	final := finalWeights(keys, changes)
	sum := final["main"] + final["a"]
	if !WeightsEqual(sum, 100) {
		t.Fatalf("fail-open sum = %v (final=%v), want 100", sum, final)
	}
	if final["main"] <= 0 || final["a"] <= 0 {
		t.Fatalf("both spares must be re-armed, got %v", final)
	}
}

func TestComputeFallbackNeverRearmsWeeklyBlockedKey(t *testing.T) {
	agents := healthyAgents()
	for i := range agents {
		agents[i] = agent(agents[i].Label, 40, 0, 20, 99, 0)
	}

	changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)
	for _, name := range []string{"opencode-go-key-1", "opencode-go-key-2", "opencode-go-key-3", "opencode-go-key-4"} {
		if final[name] != 0 {
			t.Errorf("%s = %v, want 0 (weekly-blocked, never re-armed)", name, final[name])
		}
	}
}

func TestComputeZerosKeyWhenRollingAtCeiling(t *testing.T) {
	agents := healthyAgents()
	agents[2] = agentRolling("A", 80, 2, 20, 50, 0, 99)

	changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)
	if final["opencode-go-key-3"] != 0 {
		t.Errorf("A = %v, want 0 (rolling blocker)", final["opencode-go-key-3"])
	}
}

func TestComputeKeepsKeyWhenRollingBelowThreshold(t *testing.T) {
	agents := healthyAgents()
	agents[2] = agentRolling("A", 80, 2, 20, 50, 0, 98)

	if changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents}); len(changes) != 0 {
		t.Errorf("changes = %+v, want 0 (rolling 98%% below 99%%)", changes)
	}
}

func TestComputeRollingThresholdConfigurable(t *testing.T) {
	agents := healthyAgents()
	agents[2] = agentRolling("A", 80, 2, 20, 50, 0, 92)

	if changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents}); len(changes) != 0 {
		t.Errorf("default threshold: changes = %+v, want 0", changes)
	}

	changes := Compute(Config{RollingEvictPercent: 90}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)
	if final["opencode-go-key-3"] != 0 {
		t.Errorf("threshold 90: A = %v, want 0", final["opencode-go-key-3"])
	}
}

func TestComputeFallbackNeverRearmsRollingBlockedKey(t *testing.T) {
	agents := healthyAgents()
	agents[0] = agentRolling("Main", 40, 0, 20, 50, 0, 99)
	agents[1] = agentRolling("R", 40, 0, 20, 50, 0, 99)
	agents[2] = agent("A", 100, 4, 4, 50, 0)
	agents[3] = agent("N", 100, 4, 4, 50, 0)

	changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)
	for _, name := range []string{"opencode-go-key-1", "opencode-go-key-2", "opencode-go-key-3", "opencode-go-key-4"} {
		if final[name] != 0 {
			t.Errorf("%s = %v, want 0", name, final[name])
		}
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

func TestComputeFallbackNeverRearmsBurnedKey(t *testing.T) {
	agents := healthyAgents()
	agents[0] = agent("Main", 100, 4, 4, 50, 0)
	agents[1] = agent("R", 100, 4, 4, 99, 0)
	// A + N under-burners: urgencies 3 and 10 → split normalized to 100.
	agents[2] = agent("A", 40, 0, 20, 50, 0)
	agents[3] = agent("N", 90, 0, 1, 50, 0)

	changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)
	if final["opencode-go-key-1"] != 0 || final["opencode-go-key-2"] != 0 {
		t.Errorf("burned re-armed! Main=%v R=%v", final["opencode-go-key-1"], final["opencode-go-key-2"])
	}
	// 3+10=13 → A≈23.077, N≈76.923
	if !WeightsEqual(final["opencode-go-key-3"]+final["opencode-go-key-4"], 100) {
		t.Errorf("A+N = %v, want 100", final["opencode-go-key-3"]+final["opencode-go-key-4"])
	}
	if final["opencode-go-key-4"] <= final["opencode-go-key-3"] {
		t.Errorf("N (%v) should outrank A (%v)", final["opencode-go-key-4"], final["opencode-go-key-3"])
	}
}

func TestComputeFallbackWithAllKeysBurned(t *testing.T) {
	agents := healthyAgents()
	for i := range agents {
		agents[i] = agent(agents[i].Label, 100, 4, 4, 50, 0)
	}

	changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents})
	final := finalWeights(healthyKeys(), changes)
	for _, name := range []string{"opencode-go-key-1", "opencode-go-key-2", "opencode-go-key-3", "opencode-go-key-4"} {
		if final[name] != 0 {
			t.Errorf("%s = %v, want 0", name, final[name])
		}
	}
}

func TestComputeCountsPinnedActiveSubscription(t *testing.T) {
	keys := []bifrost.Key{
		key("main", "main", "env.OPENCODE_GO_API_KEY", 1, "success"),
		key("a", "a", "env.OPENCODE_GO_API_KEY_A", 1, "success"),
	}
	agents := []quotas.Agent{
		agent("Main", 50, 0, 10, 50, 0),
		agent("A", 50, 0, 10, 99, 0),
	}
	changes := Compute(Config{Pinned: map[string]bool{"main": true}, MinActive: 1}, Input{Keys: keys, Agents: agents})
	if len(changes) != 1 || changes[0].Key.ID != "a" || changes[0].To != 0 {
		t.Fatalf("changes = %+v, want only A -> 0 because pinned Main already routes", changes)
	}
}

func TestComputeCountsDuplicateEnvRefsOnce(t *testing.T) {
	keys := []bifrost.Key{
		key("main-1", "main-1", "env.OPENCODE_GO_API_KEY", 0, "success"),
		key("main-2", "main-2", "env.OPENCODE_GO_API_KEY", 0, "success"),
		key("a", "a", "env.OPENCODE_GO_API_KEY_A", 0, "success"),
	}
	agents := []quotas.Agent{
		agent("Main", 99, 0, 10000, 50, 0),
		agent("A", 98, 0, 10000, 50, 0),
	}
	changes := Compute(Config{MinActive: 2}, Input{Keys: keys, Agents: agents})
	final := map[string]float64{"main-1": 0, "main-2": 0, "a": 0}
	for _, change := range changes {
		final[change.Key.ID] = change.To
	}
	mainArmed := 0
	var mainW float64
	for _, id := range []string{"main-1", "main-2"} {
		if final[id] > 0 {
			mainArmed++
			mainW = final[id]
		}
	}
	if mainArmed != 1 {
		t.Fatalf("final = %v, want exactly one Main row re-armed", final)
	}
	if final["a"] <= 0 {
		t.Fatalf("final = %v, want A re-armed", final)
	}
	if !WeightsEqual(mainW+final["a"], 100) {
		t.Fatalf("fail-open sum = %v, want 100", mainW+final["a"])
	}
}

func TestComputeNeverRearmsWeeklyCeiling(t *testing.T) {
	keys := []bifrost.Key{
		key("main", "main", "env.OPENCODE_GO_API_KEY", 1, "success"),
		key("a", "a", "env.OPENCODE_GO_API_KEY_A", 1, "success"),
	}
	agents := []quotas.Agent{
		agent("Main", 50, 0, 10, 100, 2),
		agent("A", 50, 0, 10, 100, 2),
	}
	changes := Compute(Config{MinActive: 2}, Input{Keys: keys, Agents: agents})
	if len(changes) != 2 {
		t.Fatalf("changes = %+v, want both keys zeroed", changes)
	}
	for _, change := range changes {
		if change.To != 0 {
			t.Fatalf("change = %+v, weekly ceiling must never be re-armed", change)
		}
	}
}

func TestComputeKeepsProjectedWeeklyExhaustionInRotation(t *testing.T) {
	keys := []bifrost.Key{
		key("main", "main", "env.OPENCODE_GO_API_KEY", 0, "success"),
		key("a", "a", "env.OPENCODE_GO_API_KEY_A", 0, "success"),
	}
	// Both under-burners (DryDays=0): urgencies 5 and 4 → ~55.556 / ~44.444.
	agents := []quotas.Agent{
		agent("Main", 50, 0, 10, 95, 2),
		agent("A", 60, 0, 10, 95, 2),
	}
	changes := Compute(Config{MinActive: 2}, Input{Keys: keys, Agents: agents})
	final := map[string]float64{}
	for _, change := range changes {
		final[change.Key.ID] = change.To
	}
	if !WeightsEqual(final["main"]+final["a"], 100) {
		t.Fatalf("final = %v, want sum 100 (weekly projection ignored)", final)
	}
	if final["main"] <= final["a"] {
		t.Fatalf("final = %v, want Main > A (urgency 5 vs 4)", final)
	}
}

func TestComputeTreatsDuplicateAgentLabelsAsUnknown(t *testing.T) {
	keys := []bifrost.Key{key("main", "main", "env.OPENCODE_GO_API_KEY", 1, "success")}
	agents := []quotas.Agent{
		agent("Main", 100, 5, 5, 100, 5),
		agent("Main", 50, 0, 10, 50, 0),
	}
	if changes := Compute(Config{MinActive: 1}, Input{Keys: keys, Agents: agents}); len(changes) != 0 {
		t.Fatalf("changes = %+v, want ambiguous quota signal left untouched", changes)
	}
}

func TestWeightsAreFiniteRoundedAndComparedAtPolicyPrecision(t *testing.T) {
	if WeightsEqual(math.NaN(), 1) || WeightsEqual(math.Inf(1), 1) {
		t.Fatal("non-finite weights must never compare equal")
	}
	if !WeightsEqual(1.235, 1.2354) {
		t.Fatal("sub-half-mill precision should compare equal")
	}
	if WeightsEqual(1.235, 1.236) {
		t.Fatal("one-mill difference should not compare equal")
	}

	keys := []bifrost.Key{key("main", "main", "env.OPENCODE_GO_API_KEY", 0, "success")}
	agents := []quotas.Agent{agent("Main", 90, 0, 8.1, 50, 0)}
	changes := Compute(Config{MinActive: 1}, Input{Keys: keys, Agents: agents})
	if len(changes) != 1 || changes[0].To != 100 {
		t.Fatalf("changes = %+v, want sole under-burner -> 100", changes)
	}

	agents[0].Windows[0].Budget.DaysLeft = math.NaN()
	if changes := Compute(Config{MinActive: 1}, Input{Keys: keys, Agents: agents}); len(changes) != 0 {
		t.Fatalf("changes = %+v, want invalid non-finite quota left untouched", changes)
	}
}

func boolPointer(value bool) *bool { return &value }

func TestComputeDoesNotCountExplicitlyDisabledSubscriptions(t *testing.T) {
	for _, test := range []struct {
		name   string
		pinned bool
	}{
		{name: "pinned", pinned: true},
		{name: "quota unknown", pinned: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			disabled := key("main", "main", "env.OPENCODE_GO_API_KEY", 1, "success")
			disabled.Enabled = boolPointer(false)
			fallback := key("a", "a", "env.OPENCODE_GO_API_KEY_A", 0, "success")
			cfg := Config{MinActive: 1}
			if test.pinned {
				cfg.Pinned = map[string]bool{"main": true}
			}
			agents := []quotas.Agent{agent("A", 50, 0, 10, 95, 1)}
			changes := Compute(cfg, Input{Keys: []bifrost.Key{disabled, fallback}, Agents: agents})
			if len(changes) != 1 || changes[0].Key.ID != "a" || changes[0].To != 100 {
				t.Fatalf("changes = %+v, want enabled fallback A -> 100", changes)
			}
		})
	}
}

func TestComputeLeavesExplicitlyDisabledManagedKeyUntouched(t *testing.T) {
	disabled := key("main", "main", "env.OPENCODE_GO_API_KEY", 2, "success")
	disabled.Enabled = boolPointer(false)
	changes := Compute(Config{MinActive: 1}, Input{
		Keys:   []bifrost.Key{disabled},
		Agents: []quotas.Agent{agent("Main", 100, 5, 5, 100, 5)},
	})
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want manually disabled key untouched", changes)
	}
}
