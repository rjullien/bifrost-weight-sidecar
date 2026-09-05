// Package engine turns Bifrost key state and OpenCode Go quota positions into
// weight changes.
//
// Weight policy — "cramer le monthly, garde-fou weekly + rolling, secours ≥ 2":
//
//   - The MONTHLY quota is lost if not consumed before the subscription
//     anniversary reset (use-it-or-lose-it). The weight of a key reflects how
//     much monthly quota remains versus how few days are left: the more quota
//     about to expire, the more traffic the key gets. It is taken out ONLY at
//     the strict ceiling (100% consumed): a key merely "projected dry" still
//     has quota that would be lost at the reset, so it keeps serving to burn
//     it. No projection-based eviction on the monthly — that would waste quota.
//   - The WEEKLY quota is a blocker, not a loss: when it hits the ceiling the
//     key stops serving until its Monday reset. The engine anticipates the
//     wall (projection) and takes the key out before it blocks.
//   - The ROLLING 5h quota is a short-lived blocker: at the ceiling the key
//     stops serving for up to 5 hours, then recovers on its own. It has no
//     pace maths (the API returns now+5h at zero usage), so it is graded on
//     raw consumption: at or above RollingEvictPercent the key is taken out,
//     and re-enters rotation by itself on a later cycle once it drops back.
//   - At least MinActive keys (2) always keep a non-zero weight as a fallback,
//     even when their urgency is low: never leave the pool without a spare.
package engine

import (
	"strings"

	"github.com/rjullien/bifrost-weight-sidecar/internal/bifrost"
	"github.com/rjullien/bifrost-weight-sidecar/internal/quotas"
)

// Config holds the policy knobs of the controller.
type Config struct {
	// Pinned lists key names (or ids) the controller must never touch:
	// manual decisions win over automation for those keys.
	Pinned map[string]bool
	// MinActive is the minimum number of keys that must keep a non-zero
	// weight at all times (fallback pool). Defaults to 2 when 0.
	MinActive int
	// RollingEvictPercent is the rolling 5h consumption (0-100) at or above
	// which a key is taken out of rotation. The rolling window is a hard
	// blocker: at that level requests start failing, so the key must not keep
	// receiving traffic. Defaults to defaultRollingEvictPercent when <= 0.
	// A value >= 100 evicts only at the strict ceiling.
	RollingEvictPercent int
}

// defaultRollingEvictPercent is the rolling 5h consumption at or above which a
// key is taken out of rotation. Set to 99%: the rolling window recovers on its
// own within ~5h, so the key is evicted only right at the ceiling, and a later
// cycle (every 10 min by default) re-enters it as soon as it drops back.
const defaultRollingEvictPercent = 99

// Change is one weight to apply.
type Change struct {
	Key  bifrost.Key
	From float64
	To   float64
}

// Input is the state snapshot of one cycle.
type Input struct {
	Keys   []bifrost.Key
	Agents []quotas.Agent
}

// rollingPercent returns the agent's rolling 5h consumption, or -1 when the
// agent is nil or carries no rolling signal (unknown).
func rollingPercent(agent *quotas.Agent) int {
	if agent == nil {
		return -1
	}
	return agent.RollingPercent()
}

// urgency is the monthly burn rate: how much monthly quota (percent) remains
// per day until the reset. The higher, the more the key must be pushed.
func urgency(agent *quotas.Agent) (float64, bool) {
	if agent == nil {
		return 0, false
	}
	pct := agent.MonthlyPercent()
	days := agent.MonthlyDaysLeft()
	if pct < 0 || days < 0 || days <= 0 {
		return 0, false
	}
	remaining := float64(100 - pct)
	if remaining < 0 {
		remaining = 0
	}
	return remaining / days, true
}

// Compute decides the target weight of every key and returns the changes
// needed. A key is left untouched (no Change) when its quotas cannot be
// assessed: acting on partial information could zero out a healthy key.
func Compute(cfg Config, in Input) []Change {
	if cfg.MinActive < 1 {
		cfg.MinActive = 2
	}
	if cfg.RollingEvictPercent <= 0 {
		cfg.RollingEvictPercent = defaultRollingEvictPercent
	}
	byLabel := make(map[string]*quotas.Agent, len(in.Agents))
	for i := range in.Agents {
		byLabel[in.Agents[i].Label] = &in.Agents[i]
	}

	// Step 1: compute the raw target for every assessable key.
	type target struct {
		key    bifrost.Key
		weight float64
	}
	var targets []target
	for _, key := range in.Keys {
		if cfg.Pinned[key.Name] || cfg.Pinned[key.ID] {
			continue
		}
		t := targetWeight(cfg, key, byLabel)
		if t < 0 {
			continue // not assessable: leave untouched
		}
		targets = append(targets, target{key: key, weight: t})
	}

	// Step 2: guarantee the fallback pool — MinActive keys must keep a
	// non-zero weight. Mettre toutes les clés à 0 tue le provider entier :
	// on garde toujours 2 clés vivantes, la première à 1 et la seconde à
	// 0.5 (poids faible mais non nul), réarmées par urgence décroissante.
	//
	// Seules les clés avec encore du monthly à cramer (urgence > 0) peuvent
	// être réarmées : une clé à 100% (monthly dry) ou morte côté Bifrost ne
	// répondra pas — lui rendre un poids ne ferait qu'envoyer du trafic vers
	// une clé en échec. Le fallback ne ressuscite que ce qui peut servir.
	// Une clé bloquée sur le rolling 5h est dans le même cas : elle échoue
	// tout de suite, donc on ne la réarme pas non plus.
	active := 0
	for _, t := range targets {
		if t.weight > 0 {
			active++
		}
	}
	if active < cfg.MinActive {
		// Sort candidates by urgency descending so the most urgent keys
		// become the fallback pool.
		for i := 0; i < len(targets)-1; i++ {
			for j := i + 1; j < len(targets); j++ {
				ui, _ := urgency(byLabel[quotasLabel(targets[i].key)])
				uj, _ := urgency(byLabel[quotasLabel(targets[j].key)])
				if uj > ui {
					targets[i], targets[j] = targets[j], targets[i]
				}
			}
		}
		// Re-arm keys with burnable monthly quota (urgency > 0) until
		// MinActive are alive: first at 1, second at 0.5 — a live spare
		// carrying little traffic, never 0. Keys killed by Bifrost health or
		// with no monthly quota left are NOT re-armed: they would fail.
		for i := 0; i < len(targets) && active < cfg.MinActive; i++ {
			if targets[i].weight > 0 || targets[i].key.Status != "success" {
				continue
			}
			agent := byLabel[quotasLabel(targets[i].key)]
			// A key blocked on the rolling 5h window fails right now: never
			// re-arm it, exactly like a monthly-dry or Bifrost-unhealthy key.
			if r := rollingPercent(agent); r >= 0 && r >= cfg.RollingEvictPercent {
				continue
			}
			u, ok := urgency(agent)
			if !ok || u <= 0 {
				continue // no monthly quota left to burn: really dead
			}
			if active == 0 {
				targets[i].weight = 1
			} else {
				targets[i].weight = 0.5
			}
			active++
		}
	}

	// Step 3: diff against the current weights.
	var changes []Change
	for _, t := range targets {
		if t.weight != t.key.Weight {
			changes = append(changes, Change{Key: t.key, From: t.key.Weight, To: t.weight})
		}
	}
	return changes
}

// quotasLabel maps a key's value ref to its agent label.
func quotasLabel(key bifrost.Key) string {
	return LabelFromEnv(key.Value.Ref)
}

// targetWeight returns the weight a key should have, or -1 when the state is
// not assessable (quota data missing for this key).
//
// Rules, in priority order:
//  1. Bifrost reports the key as not healthy  → 0 (dead key)
//  2. rolling 5h at/above the evict threshold → 0 (blocked right now, ~5h)
//  3. weekly projected dry (dryDays > 0)     → 0 (will block before Monday)
//  4. monthly at the ceiling (100% consumed) → 0 (nothing left to burn)
//  5. otherwise                              → urgency (monthly remaining /
//     days left) — the more quota about to expire, the more traffic.
//
// The monthly window is graded at the STRICT ceiling (100%), not on a
// projection: the monthly quota is lost if not consumed before the anniversary
// reset (use-it-or-lose-it), so a key merely "projected dry" must keep serving
// to burn what remains. Only the weekly, a blocker rather than a loss, is
// anticipated on its projection.
func targetWeight(cfg Config, key bifrost.Key, byLabel map[string]*quotas.Agent) float64 {
	// Rule 1: Bifrost's own key health. Applies even when the quota data
	// is missing for this key.
	if key.Status != "success" {
		return 0
	}

	agent, ok := byLabel[LabelFromEnv(key.Value.Ref)]
	// The OpenCode Go API does not follow this key, or failed to fetch it.
	if !ok || agent == nil || agent.Error != "" {
		return -1
	}

	// Rule 2: rolling 5h blocker. Graded on raw consumption (no pace maths):
	// at/above the threshold the key is blocked right now and would only serve
	// failures. It recovers on its own within ~5h, so a later cycle re-enters
	// it via the urgency path once the rolling drops back below the threshold.
	if rolling := agent.RollingPercent(); rolling >= 0 && rolling >= cfg.RollingEvictPercent {
		return 0
	}

	weeklyDry := agent.WeeklyDryDays()

	// Rule 3: weekly blocker projected.
	if weeklyDry >= 0 && weeklyDry > 0 {
		return 0
	}

	// Rule 4: monthly quota exhausted — STRICT ceiling only. A key merely
	// "projected dry" still has monthly quota that would be LOST at the reset:
	// keep it serving (rule 5) to burn it. Only 100% means nothing left.
	if monthly := agent.MonthlyPercent(); monthly >= 100 {
		return 0
	}

	// Rule 5: burn the monthly — weight proportional to what would be lost.
	if u, ok := urgency(agent); ok {
		return u
	}
	return -1 // no monthly signal: leave untouched
}

// LabelFromEnv derives the display label from a Bifrost key value
// ref, mirroring the dashboard's key discovery: "env.OPENCODE_GO_API_KEY_A"
// is the "A" subscription, "env.OPENCODE_GO_API_KEY" is "Main".
func LabelFromEnv(ref string) string {
	const prefix = "env.OPENCODE_GO_API_KEY"
	if ref == prefix {
		return "Main"
	}
	const separator = prefix + "_"
	if !strings.HasPrefix(ref, separator) {
		return ""
	}
	label := strings.TrimPrefix(ref, separator)
	if label == "" {
		return ""
	}
	return label
}
