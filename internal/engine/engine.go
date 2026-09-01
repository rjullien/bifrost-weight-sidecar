// Package engine turns Bifrost key state and OpenCode Go quota positions into
// weight changes.
package engine

import (
	"strings"

	"github.com/rjullien/bifrost-weight-sidecar/internal/bifrost"
	"github.com/rjullien/bifrost-weight-sidecar/internal/quotas"
)

// Config holds the policy knobs of the controller.
type Config struct {
	// WeeklyThreshold is the weekly consumption (percent) above which a key
	// is taken out of rotation. 0 disables the rule.
	WeeklyThreshold int
	// Pinned lists key names (or ids) the controller must never touch:
	// manual decisions win over automation for those keys.
	Pinned map[string]bool
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

// Compute decides the target weight of every key and returns the changes
// needed. A key is left untouched (no Change) when its quotas cannot be
// assessed: acting on partial information could zero out a healthy key.
func Compute(cfg Config, in Input) []Change {
	byLabel := make(map[string]quotas.Agent, len(in.Agents))
	for _, a := range in.Agents {
		byLabel[a.Label] = a
	}

	var changes []Change
	for _, key := range in.Keys {
		if cfg.Pinned[key.Name] || cfg.Pinned[key.ID] {
			continue
		}
		target := targetWeight(cfg, key, byLabel)
		if target < 0 {
			continue // not assessable: leave untouched
		}
		if target != key.Weight {
			changes = append(changes, Change{Key: key, From: key.Weight, To: target})
		}
	}
	return changes
}

// targetWeight returns the weight a key should have, or -1 when the state is
// not assessable (quota data missing for this key).
//
// Rules, in priority order:
//  1. Bifrost reports the key as not healthy  → 0 (dead key)
//  2. weekly consumption ≥ threshold          → 0 (about to hit a ceiling)
//  3. monthly quota projected dry (dryDays>0) → 0 (will sit at the ceiling)
//  4. otherwise                               → 1 (back in rotation)
func targetWeight(cfg Config, key bifrost.Key, byLabel map[string]quotas.Agent) float64 {
	// Rule 1: Bifrost's own key health. Applies even when the quota data
	// is missing for this key.
	if key.Status != "" && key.Status != "success" {
		return 0
	}

	agent, ok := byLabel[LabelFromEnv(key.Value.Ref)]
	// The OpenCode Go API does not follow this key, or failed to fetch it.
	if !ok || agent.Error != "" {
		return -1
	}

	weekly := agent.WeeklyPercent()
	monthly := agent.MonthlyDryDays()

	// Rules 2 and 3 need at least one usable quota signal.
	if weekly < 0 && monthly < 0 {
		return -1
	}

	if cfg.WeeklyThreshold > 0 && weekly >= 0 && weekly >= cfg.WeeklyThreshold {
		return 0
	}
	if monthly >= 0 && monthly > 0 {
		return 0
	}
	return 1
}

// LabelFromEnv derives the display label from a Bifrost key value
// ref, mirroring the dashboard's key discovery: "env.OPENCODE_GO_API_KEY_A"
// is the "A" subscription, "env.OPENCODE_GO_API_KEY" is "Main".
func LabelFromEnv(ref string) string {
	const prefix = "env.OPENCODE_GO_API_KEY"
	if !strings.HasPrefix(ref, prefix) {
		return ""
	}
	suffix := strings.TrimPrefix(ref, prefix)
	if suffix == "" {
		return "Main"
	}
	return strings.TrimPrefix(suffix, "_")
}
