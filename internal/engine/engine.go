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
//     key stops serving until its Monday reset. It is graded on raw
//     consumption at a near-ceiling threshold (WeeklyEvictPercent, 99% by
//     default) and taken out — no projection, like the rolling window.
//   - The ROLLING 5h quota is a short-lived blocker: at the ceiling the key
//     stops serving for up to 5 hours, then recovers on its own. It has no
//     pace maths (the API returns now+5h at zero usage), so it is graded on
//     raw consumption: at or above RollingEvictPercent the key is taken out,
//     and re-enters rotation by itself on a later cycle once it drops back.
//   - MinActive counts distinct enabled healthy subscriptions, including pinned
//     and untouched healthy keys; duplicate Bifrost entries sharing one env
//     ref count once. The fallback may re-arm a key that still has burnable
//     monthly quota, but never one already at a weekly/rolling/monthly blocker
//     or unhealthy in Bifrost.
package engine

import (
	"math"
	"sort"
	"strings"

	"github.com/rjullien/bifrost-weight-sidecar/internal/bifrost"
	"github.com/rjullien/bifrost-weight-sidecar/internal/quotas"
)

const weightScale = 1000.0

// Config holds the policy knobs of the controller.
type Config struct {
	// Pinned lists key names (or ids) the controller must never touch:
	// manual decisions win over automation for those keys.
	Pinned map[string]bool
	// MinActive is the minimum number of distinct healthy subscriptions that
	// should keep a non-zero weight. Defaults to 2 when below 1.
	MinActive int
	// RollingEvictPercent is the rolling 5h consumption (0-100) at or above
	// which a key is taken out of rotation. The rolling window is a hard
	// blocker: at that level requests start failing, so the key must not keep
	// receiving traffic. Defaults to defaultRollingEvictPercent when <= 0.
	// A value >= 100 evicts only at the strict ceiling.
	RollingEvictPercent int
	// WeeklyEvictPercent is the weekly consumption (0-100) at or above which a
	// key is taken out of rotation. Like the rolling window, the weekly is a
	// blocker graded on raw consumption, not on a projection. Defaults to
	// defaultWeeklyEvictPercent when <= 0.
	WeeklyEvictPercent int
}

// defaultRollingEvictPercent is the rolling 5h consumption at or above which a
// key is taken out of rotation. Set to 99%: the rolling window recovers on its
// own within ~5h, so the key is evicted only right at the ceiling, and a later
// cycle (every 10 min by default) re-enters it as soon as it drops back.
const defaultRollingEvictPercent = 99

// defaultWeeklyEvictPercent is the weekly consumption at or above which a key
// is taken out of rotation. Set to 99%, matching the rolling window: the
// weekly is a blocker (the key stops serving until its Monday reset), so it is
// evicted right at the ceiling on raw consumption rather than anticipated on a
// projection.
const defaultWeeklyEvictPercent = 99

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

// WeightsEqual compares controller weights at the precision persisted by the
// policy. Exported so the orchestration layer can detect concurrent changes.
func WeightsEqual(a, b float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) {
		return false
	}
	return math.Abs(a-b) < 0.5/weightScale
}

func normalizeWeight(weight float64) (float64, bool) {
	if weight < 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
		return 0, false
	}
	return math.Round(weight*weightScale) / weightScale, true
}

// urgency is the monthly burn rate: how much monthly quota remains per day.
func urgency(agent *quotas.Agent) (float64, bool) {
	if agent == nil || agent.Error != "" {
		return 0, false
	}
	pct := agent.MonthlyPercent()
	days := agent.MonthlyDaysLeft()
	if pct < 0 || pct > 100 || days <= 0 || math.IsNaN(days) || math.IsInf(days, 0) {
		return 0, false
	}
	return normalizeWeight(float64(100-pct) / days)
}

// Compute decides the target weight of every managed key. Healthy keys whose
// quotas cannot be assessed and pinned keys are left untouched, but still
// count toward the fallback pool when already active.
func Compute(cfg Config, in Input) []Change {
	if cfg.MinActive < 1 {
		cfg.MinActive = 2
	}
	if cfg.RollingEvictPercent <= 0 {
		cfg.RollingEvictPercent = defaultRollingEvictPercent
	}
	if cfg.WeeklyEvictPercent <= 0 {
		cfg.WeeklyEvictPercent = defaultWeeklyEvictPercent
	}

	// Duplicate labels are ambiguous. Mark them nil so no ordering-dependent
	// quota decision can be made for that subscription.
	byLabel := make(map[string]*quotas.Agent, len(in.Agents))
	for i := range in.Agents {
		label := in.Agents[i].Label
		if _, exists := byLabel[label]; exists {
			byLabel[label] = nil
			continue
		}
		byLabel[label] = &in.Agents[i]
	}

	type target struct {
		inputIndex int
		key        bifrost.Key
		label      string
		agent      *quotas.Agent
		weight     float64
	}

	// effective starts from the actual pool state and is overwritten only for
	// assessable, non-pinned keys managed through OPENCODE_GO_API_KEY refs.
	effective := make([]float64, len(in.Keys))
	for i := range in.Keys {
		effective[i] = in.Keys[i].Weight
	}

	var targets []target
	for i, key := range in.Keys {
		label := LabelFromEnv(key.Value.Ref)
		if label == "" || cfg.Pinned[key.Name] || cfg.Pinned[key.ID] {
			continue
		}
		weight := targetWeight(cfg, key, byLabel)
		if weight < 0 {
			continue
		}
		effective[i] = weight
		targets = append(targets, target{
			inputIndex: i,
			key:        key,
			label:      label,
			agent:      byLabel[label],
			weight:     weight,
		})
	}

	// Count distinct enabled healthy subscriptions in the effective final state.
	// This includes pinned and quota-unknown keys and collapses duplicate refs.
	active := make(map[string]bool)
	for i, key := range in.Keys {
		if key.Status != "success" || !routingEnabled(key) || effective[i] <= 0 {
			continue
		}
		active[subscriptionIdentity(key)] = true
	}

	if len(active) < cfg.MinActive {
		sort.SliceStable(targets, func(i, j int) bool {
			ui, _ := urgency(targets[i].agent)
			uj, _ := urgency(targets[j].agent)
			return ui > uj
		})

		for i := range targets {
			if len(active) >= cfg.MinActive {
				break
			}
			t := &targets[i]
			identity := subscriptionIdentity(t.key)
			if t.weight > 0 || active[identity] || !fallbackEligible(cfg, t.key, t.agent) {
				continue
			}
			if len(active) == 0 {
				t.weight = 1
			} else {
				t.weight = 0.5
			}
			effective[t.inputIndex] = t.weight
			active[identity] = true
		}
	}

	var changes []Change
	for _, t := range targets {
		if !WeightsEqual(t.weight, t.key.Weight) {
			changes = append(changes, Change{Key: t.key, From: t.key.Weight, To: t.weight})
		}
	}
	return changes
}

func routingEnabled(key bifrost.Key) bool {
	return key.Enabled == nil || *key.Enabled
}

func subscriptionIdentity(key bifrost.Key) string {
	if label := LabelFromEnv(key.Value.Ref); label != "" {
		return "quota:" + label
	}
	return "key:" + key.ID
}

// fallbackEligible permits a last-resort re-arm only while quota remains.
// Weekly/rolling blockers (raw consumption at/above threshold), an already
// reached monthly ceiling, and unhealthy Bifrost status are hard stops.
func fallbackEligible(cfg Config, key bifrost.Key, agent *quotas.Agent) bool {
	if key.Status != "success" || !routingEnabled(key) || agent == nil || agent.Error != "" {
		return false
	}
	if agent.MonthlyPercent() >= 100 {
		return false
	}
	if rolling := agent.RollingPercent(); rolling >= 0 && rolling >= cfg.RollingEvictPercent {
		return false
	}
	if weekly := agent.WeeklyPercent(); weekly >= 0 && weekly >= cfg.WeeklyEvictPercent {
		return false
	}
	// A readable monthly burn signal is enough, even when the rounded urgency
	// weight is 0: the fallback can still bump the key to 1 / 0.5.
	_, ok := urgency(agent)
	return ok
}

// targetWeight returns a non-negative target, or -1 when the state is not
// assessable. It is called only for managed env references.
//
// Rules, in priority order:
//  1. Bifrost reports the key as not healthy   → 0 (dead key)
//  2. rolling 5h at/above RollingEvictPercent  → 0 (blocked right now, ~5h)
//  3. weekly at/above WeeklyEvictPercent       → 0 (blocked until Monday)
//  4. monthly at the ceiling (100% consumed)   → 0 (nothing left to burn)
//  5. otherwise                                → urgency (monthly remaining /
//     days left) — the more quota about to expire, the more traffic.
func targetWeight(cfg Config, key bifrost.Key, byLabel map[string]*quotas.Agent) float64 {
	if !routingEnabled(key) {
		return -1
	}
	if key.Status != "success" {
		return 0
	}

	agent := byLabel[LabelFromEnv(key.Value.Ref)]
	if agent == nil || agent.Error != "" {
		return -1
	}

	// Rule 2: rolling 5h blocker. Graded on raw consumption (no pace maths).
	if rolling := agent.RollingPercent(); rolling >= 0 && rolling >= cfg.RollingEvictPercent {
		return 0
	}

	// Rule 3: weekly blocker. Graded on raw consumption, like the rolling
	// window — no projection-based eviction.
	if weekly := agent.WeeklyPercent(); weekly >= 0 && weekly >= cfg.WeeklyEvictPercent {
		return 0
	}

	// Rule 4: monthly quota exhausted — STRICT ceiling only.
	if monthly := agent.MonthlyPercent(); monthly >= 100 {
		return 0
	}

	// Rule 5: burn the monthly — weight proportional to what would be lost.
	if u, ok := urgency(agent); ok {
		return u
	}
	return -1
}

// LabelFromEnv derives the subscription label from a Bifrost env reference.
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
