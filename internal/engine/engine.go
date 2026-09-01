// Package engine turns Bifrost key state and OpenCode Go quota positions into
// weight changes.
//
// Weight policy — "cramer le monthly, garde-fou weekly, secours ≥ 2":
//
//   - The MONTHLY quota is lost if not consumed before the subscription
//     anniversary reset (use-it-or-lose-it). The weight of a key reflects how
//     much monthly quota remains versus how few days are left: the more quota
//     about to expire, the more traffic the key gets. When at the ceiling
//     (100%), there is nothing left to burn: weight 0.
//   - The WEEKLY quota is a blocker, not a loss: when it hits the ceiling the
//     key stops serving until its Monday reset. The engine anticipates the
//     wall (projection) and takes the key out before it blocks.
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
}

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
//  2. weekly projected dry (dryDays > 0)     → 0 (will block before Monday)
//  3. monthly at the ceiling (dryDays > 0)   → 0 (nothing left to burn)
//  4. otherwise                              → urgency (monthly remaining /
//     days left) — the more quota about to expire, the more traffic.
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

	weeklyDry := agent.WeeklyDryDays()
	monthlyDry := agent.MonthlyDryDays()

	// Rule 2: weekly blocker projected.
	if weeklyDry >= 0 && weeklyDry > 0 {
		return 0
	}

	// Rule 3: monthly quota exhausted (or projected dry until the reset).
	if monthlyDry >= 0 && monthlyDry > 0 {
		return 0
	}

	// Rule 4: burn the monthly — weight proportional to what would be lost.
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
