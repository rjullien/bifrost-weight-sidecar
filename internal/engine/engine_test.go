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
// days left + weekly percent + weekly dry days. Rolling 5h defaults to 0.
func agent(label string, monthlyPct int, monthlyDry, monthlyDaysLeft float64, weeklyPct int, weeklyDry float64) quotas.Agent {
	return agentRolling(label, monthlyPct, monthlyDry, monthlyDaysLeft, weeklyPct, weeklyDry, 0)
}

// agentRolling is agent() plus an explicit rolling 5h percent, for the rolling
// eviction tests.
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

// Monthly quota exhausted at the STRICT ceiling (100%) → weight 0 (nothing
// left to burn).
func TestComputeZerosKeyWhenMonthlyDry(t *testing.T) {
	cfg := Config{}
	agents := healthyAgents()
	agents[1] = agent("R", 100, 5, 5, 50, 0) // R at 100% for 5 days

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	if changes[0].Key.Name != "opencode-go-key-2" || changes[0].To != 0 {
		t.Errorf("change = %+v, want key-2 -> 0", changes[0])
	}
}

// A key PROJECTED dry on the monthly but still below 100% must keep serving:
// the remaining monthly quota is lost at the reset (use-it-or-lose-it), so
// evicting it would waste quota. Only the strict 100% ceiling evicts.
func TestComputeKeepsMonthlyProjectedDryButBelowCeiling(t *testing.T) {
	cfg := Config{}
	agents := healthyAgents()
	// R at 70%, projected dry in a few days (DryDays=3) — but 30% quota left
	// to burn before the reset. Must NOT be zeroed; instead it burns faster
	// (higher urgency) than the healthy keys.
	agents[1] = agent("R", 70, 3, 2, 50, 0) // 30% left / 2 days → urgency 15

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	var rTo float64
	rSeen := false
	for _, c := range changes {
		if c.Key.Name == "opencode-go-key-2" {
			rTo, rSeen = c.To, true
		}
	}
	if !rSeen {
		t.Fatal("R should change weight (higher urgency), got no change")
	}
	if rTo == 0 {
		t.Error("R zeroed while below 100% — quota would be wasted; want > 0")
	}
	if rTo != 15 {
		t.Errorf("R to = %v, want 15 (30%% left / 2 days), burning the monthly", rTo)
	}
}

// Weekly at/above the evict threshold → 0 even with monthly quota left: the
// key is blocked until Monday. Graded on raw consumption, not on a projection.
func TestComputeZerosKeyWhenWeeklyBlocks(t *testing.T) {
	cfg := Config{}
	agents := healthyAgents()
	agents[2] = agent("A", 40, 0, 20, 99, 0) // weekly at 99% → blocked

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	if changes[0].Key.Name != "opencode-go-key-3" || changes[0].To != 0 {
		t.Errorf("change = %+v, want key-3 -> 0 (weekly blocker)", changes[0])
	}
}

// Weekly projected dry but still below the threshold must NOT evict on the
// projection: only raw consumption at/above the threshold blocks. A high burn
// rate alone (dryDays > 0) no longer takes the key out.
func TestComputeKeepsKeyWhenWeeklyProjectedDryButBelowThreshold(t *testing.T) {
	cfg := Config{}
	agents := healthyAgents()
	// Monthly 80%/20j → urgency 1 = current weight, so any diff would come from
	// the weekly rule alone. Weekly 80% (projected dry) must NOT evict.
	agents[2] = agent("A", 80, 0, 20, 80, 1.5)

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want 0 (weekly 80%% < 99%%, projection ignored)", changes)
	}
}

// The weekly eviction threshold is configurable, symmetric to the rolling one.
func TestComputeWeeklyThresholdConfigurable(t *testing.T) {
	agents := healthyAgents()
	// Monthly 80%/20j → urgency 1 = current weight: isolate the weekly rule.
	agents[2] = agent("A", 80, 0, 20, 92, 0) // weekly 92%

	// Default threshold (99): 92% stays in rotation.
	if changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents}); len(changes) != 0 {
		t.Errorf("default threshold: changes = %+v, want 0 (92%% < 99%%)", changes)
	}

	// Lower threshold (90): 92% is now evicted.
	changes := Compute(Config{WeeklyEvictPercent: 90}, Input{Keys: healthyKeys(), Agents: agents})
	if len(changes) != 1 || changes[0].Key.Name != "opencode-go-key-3" || changes[0].To != 0 {
		t.Errorf("threshold 90: changes = %+v, want key-3 -> 0", changes)
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

// Even when every key is blocked (weekly at the ceiling or monthly at 100%),
// MinActive keys must keep a non-zero weight: the pool never loses its spare.
// Re-armed keys get 1 then 0.5 (a live spare carrying little traffic, never 0).
//
// The re-armed spares must be keys that can actually serve: a weekly-blocked
// or monthly-dry key is never resurrected. Here Main and A keep burnable
// monthly quota with a healthy weekly, so they become the spares.
func TestComputeKeepsAtLeastMinActiveKeysAsFallback(t *testing.T) {
	cfg := Config{}
	agents := healthyAgents()
	agents[0] = agent("Main", 95, 0, 1, 50, 0) // healthy weekly, monthly burnable
	agents[1] = agent("R", 100, 4, 4, 99, 0)   // weekly ceiling + monthly ceiling
	agents[2] = agent("A", 40, 0, 20, 50, 0)   // healthy weekly, monthly burnable
	agents[3] = agent("N", 100, 4, 4, 99, 0)   // weekly ceiling + monthly ceiling

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	// État final : poids initial (1) + changements appliqués.
	final := map[string]float64{}
	for _, k := range healthyKeys() {
		final[k.Name] = k.Weight
	}
	for _, c := range changes {
		final[c.Key.Name] = c.To
	}
	// R and N are doubly blocked (weekly ceiling + monthly 100%): always 0.
	if final["opencode-go-key-2"] != 0 || final["opencode-go-key-4"] != 0 {
		t.Errorf("blocked keys alive! R=%v N=%v, want 0/0", final["opencode-go-key-2"], final["opencode-go-key-4"])
	}
	// Main and A can serve (healthy weekly, monthly burnable) → at least two
	// keys keep a non-zero weight, so the pool never loses its spare.
	alive := 0
	for _, v := range final {
		if v > 0 {
			alive++
		}
	}
	if alive < 2 {
		t.Errorf("alive keys = %d (final=%v), want >= 2 (pool keeps a spare)", alive, final)
	}
}

// When EVERY key is blocked (weekly at the ceiling), the fallback must NOT
// resurrect any of them: re-arming a weekly-blocked key only routes traffic to
// a key that fails until Monday. The pool is left degraded on purpose.
func TestComputeFallbackNeverRearmsWeeklyBlockedKey(t *testing.T) {
	cfg := Config{}
	agents := healthyAgents()
	// All four at the weekly ceiling but with monthly quota still burnable:
	// urgency > 0, yet they must stay at 0 (weekly blocks them right now).
	for i := range agents {
		agents[i] = agent(agents[i].Label, 40, 0, 20, 99, 0)
	}

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	to := map[string]float64{}
	for _, c := range changes {
		to[c.Key.Name] = c.To
	}
	for _, name := range []string{"opencode-go-key-1", "opencode-go-key-2", "opencode-go-key-3", "opencode-go-key-4"} {
		if to[name] != 0 {
			t.Errorf("%s = %v, want 0 (weekly-blocked, never re-armed)", name, to[name])
		}
	}
}

// Rolling 5h at/above the evict threshold → weight 0: the key is blocked right
// now and would only serve failures until the ~5h window recovers.
func TestComputeZerosKeyWhenRollingAtCeiling(t *testing.T) {
	cfg := Config{}
	agents := healthyAgents()
	agents[2] = agentRolling("A", 80, 0, 20, 50, 0, 99) // rolling at the ceiling

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	if changes[0].Key.Name != "opencode-go-key-3" || changes[0].To != 0 {
		t.Errorf("change = %+v, want key-3 -> 0 (rolling blocker)", changes[0])
	}
}

// Rolling just below the threshold must NOT evict: the key still serves.
func TestComputeKeepsKeyWhenRollingBelowThreshold(t *testing.T) {
	cfg := Config{} // default threshold 99
	agents := healthyAgents()
	agents[2] = agentRolling("A", 80, 0, 20, 50, 0, 98) // 98% < 99%

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want 0 (rolling 98%% below 99%% threshold)", changes)
	}
}

// The eviction threshold is configurable: at RollingEvictPercent=90 a key at
// 92% is evicted, whereas the default (99) would keep it.
func TestComputeRollingThresholdConfigurable(t *testing.T) {
	agents := healthyAgents()
	agents[2] = agentRolling("A", 80, 0, 20, 50, 0, 92)

	// Default threshold (99): 92% stays in rotation.
	if changes := Compute(Config{}, Input{Keys: healthyKeys(), Agents: agents}); len(changes) != 0 {
		t.Errorf("default threshold: changes = %+v, want 0 (92%% < 99%%)", changes)
	}

	// Lower threshold (90): 92% is now evicted.
	changes := Compute(Config{RollingEvictPercent: 90}, Input{Keys: healthyKeys(), Agents: agents})
	if len(changes) != 1 || changes[0].Key.Name != "opencode-go-key-3" || changes[0].To != 0 {
		t.Errorf("threshold 90: changes = %+v, want key-3 -> 0", changes)
	}
}

// A key blocked ONLY by the rolling window (no weekly/monthly dry, monthly
// quota still burnable) must never be re-armed by the fallback: it would serve
// failures for the next ~5h. This isolates the rolling skip as the sole reason
// the key is not resurrected — a weekly/monthly-dry key would be skipped for
// other reasons too.
func TestComputeFallbackNeverRearmsRollingBlockedKey(t *testing.T) {
	cfg := Config{}
	agents := healthyAgents()
	// Main + R: rolling at ceiling, but monthly has room and weekly is fine.
	// Their only blocker is the rolling window. Without the rolling skip, the
	// fallback would happily re-arm them (urgency > 0).
	agents[0] = agentRolling("Main", 40, 0, 20, 50, 0, 99) // urgency 60/20 = 3
	agents[1] = agentRolling("R", 40, 0, 20, 50, 0, 99)    // urgency 60/20 = 3
	// A + N: monthly ceiling (dry) — dead for good, cannot be the spares.
	agents[2] = agent("A", 100, 4, 4, 50, 0)
	agents[3] = agent("N", 100, 4, 4, 50, 0)

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	final := map[string]float64{}
	for _, k := range healthyKeys() {
		final[k.Name] = k.Weight
	}
	for _, c := range changes {
		final[c.Key.Name] = c.To
	}
	// Every key must end at 0: the only keys with burnable monthly (Main, R)
	// are rolling-blocked and must NOT be re-armed; A and N are monthly-dry.
	// The pool is legitimately left with no spare rather than routing to keys
	// that would fail right now.
	for _, name := range []string{"opencode-go-key-1", "opencode-go-key-2", "opencode-go-key-3", "opencode-go-key-4"} {
		if final[name] != 0 {
			t.Errorf("%s = %v, want 0 (rolling-blocked or monthly-dry, never re-armed)", name, final[name])
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

// Une clé à 100% (monthly cramé, plus rien à brûler) ne doit JAMAIS être
// réarmée par le fallback : lui rendre un poids enverrait du trafic vers une
// clé qui échoue. Seule une clé avec encore du monthly à cramer (urgence > 0)
// peut être ressuscitée.
func TestComputeFallbackNeverRearmsBurnedKey(t *testing.T) {
	cfg := Config{}
	agents := healthyAgents()
	// Main: monthly cramé (100%) — mort pour de bon.
	agents[0] = agent("Main", 100, 4, 4, 50, 0)
	// R: weekly au plafond (99%) + monthly cramé — mort.
	agents[1] = agent("R", 100, 4, 4, 99, 0)
	// A: weekly sain, monthly à 40% (60% à brûler, urgence 60/20=3) → vivable.
	agents[2] = agent("A", 40, 0, 20, 50, 0)
	// N: weekly sain, monthly à 90% (10% à brûler, urgence 10/1=10) → vivable.
	agents[3] = agent("N", 90, 0, 1, 50, 0)

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	// État final : poids initial (1) + changements appliqués.
	final := map[string]float64{}
	for _, k := range healthyKeys() {
		final[k.Name] = k.Weight
	}
	for _, c := range changes {
		final[c.Key.Name] = c.To
	}
	// Les clés cramées (100% ou weekly au plafond) restent à 0 — jamais réarmées.
	if final["opencode-go-key-1"] != 0 || final["opencode-go-key-2"] != 0 {
		t.Errorf("cramées réarmées ! Main=%v R=%v, want 0/0 (ne jamais ressusciter une clé bloquée)", final["opencode-go-key-1"], final["opencode-go-key-2"])
	}
	// Les deux clés vivables servent (poids = leur urgence : A=3, N=10). Elles
	// portent le trafic sans que le fallback ait à intervenir.
	if final["opencode-go-key-3"] != 3 {
		t.Errorf("A final = %v, want 3 (urgence 60%%/20j)", final["opencode-go-key-3"])
	}
	if final["opencode-go-key-4"] != 10 {
		t.Errorf("N final = %v, want 10 (urgence 10%%/1j)", final["opencode-go-key-4"])
	}
}

// Toutes les clés cramées → aucune réarmée : le pool est réellement mort, le
// fallback ne doit pas envoyer de trafic vers des clés qui échouent.
func TestComputeFallbackWithAllKeysBurned(t *testing.T) {
	cfg := Config{}
	agents := healthyAgents()
	for i := range agents {
		agents[i] = agent(agents[i].Label, 100, 4, 4, 50, 0) // tout à 100%
	}

	changes := Compute(cfg, Input{Keys: healthyKeys(), Agents: agents})
	to := map[string]float64{}
	for _, c := range changes {
		to[c.Key.Name] = c.To
	}
	for _, name := range []string{"opencode-go-key-1", "opencode-go-key-2", "opencode-go-key-3", "opencode-go-key-4"} {
		if to[name] != 0 {
			t.Errorf("%s = %v, want 0 (clé cramée — jamais réarmée même si tout est mort)", name, to[name])
		}
	}
}
