// Package engine turns Bifrost key state and OpenCode Go quota positions into
// weight changes.
//
// Weight policy — "burn monthly to 100% by J−1 (reset−BurnLead), blockers,
// fail-open spare":
//
//   - The MONTHLY quota is lost if not consumed before the subscription
//     anniversary reset (use-it-or-lose-it). The goal is to finish every
//     assessable key to 100% one full BurnLead (default 24h) before that
//     reset — the J−1 burn wall used by quotas.MonthlyDryDays.
//   - Keys projected to hit 100% by the wall (MonthlyDryDays > 0) do not need
//     burn-priority traffic. Keys that still have remaining monthly AND will
//     NOT hit 100% by the wall (MonthlyDryDays == 0) are under-burners:
//     when any exist, they receive all weight (winner-take-all / split by
//     urgency), normalized so active targets sum to 100.
//   - The WEEKLY and ROLLING 5h quotas are hard blockers graded on raw
//     consumption at near-ceiling thresholds (99% by default).
//   - MinActive fail-open only when the pool would otherwise have zero
//     routable keys: it must not dilute a single under-burner that needs 100%.
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
	// MinActive is the minimum number of distinct healthy subscriptions to
	// re-arm when the managed pool would otherwise have zero routable keys.
	// Defaults to 2 when below 1. It does not force a second key when an
	// under-burner already holds the burn-to-100% allocation.
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

// urgency is the monthly burn rate: remaining monthly percent per day left.
func urgency(agent *quotas.Agent) (float64, bool) {
	if agent == nil || agent.Error != "" {
		return 0, false
	}
	pct := agent.MonthlyPercent()
	days := agent.MonthlyDaysLeft()
	if pct < 0 || pct > 100 || days <= 0 || math.IsNaN(days) || math.IsInf(days, 0) {
		return 0, false
	}
	raw := float64(100-pct) / days
	if math.IsNaN(raw) || math.IsInf(raw, 0) || raw < 0 {
		return 0, false
	}
	return normalizeWeight(raw)
}

// underBurner is a key with remaining monthly quota that will NOT reach 100%
// by the J−1 burn wall (resetsAt−BurnLead) at the current pace
// (MonthlyDryDays == 0). MonthlyDryDays > 0 means on track for that wall;
// -1 means unknown.
func underBurner(agent *quotas.Agent) bool {
	if agent == nil || agent.Error != "" {
		return false
	}
	pct := agent.MonthlyPercent()
	if pct < 0 || pct >= 100 {
		return false
	}
	dry := agent.MonthlyDryDays()
	return dry == 0
}

// normalizeScoresToHundred turns raw positive scores into percentage weights
// that sum to exactly 100 (weightScale rounding; largest absorbs the delta).
func normalizeScoresToHundred(scores []float64) []float64 {
	out := make([]float64, len(scores))
	var sum float64
	for _, s := range scores {
		if s > 0 {
			sum += s
		}
	}
	if sum <= 0 {
		return out
	}

	var roundedSum float64
	largest := -1
	for i, s := range scores {
		if s <= 0 {
			continue
		}
		w, ok := normalizeWeight(100 * s / sum)
		if !ok || w <= 0 {
			continue
		}
		out[i] = w
		roundedSum += w
		if largest < 0 || out[i] > out[largest] {
			largest = i
		}
	}
	if largest >= 0 {
		delta := 100 - roundedSum
		if adj, ok := normalizeWeight(out[largest] + delta); ok && adj > 0 {
			out[largest] = adj
		}
	}
	return out
}

// Compute decides the target weight of every managed key. Healthy keys whose
// quotas cannot be assessed and pinned keys are left untouched, but still
// count toward the fail-open pool when already active.
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
		blocked    bool
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
		status := assessKey(cfg, key, byLabel)
		if status == assessSkip {
			continue
		}
		agent := byLabel[label]
		t := target{
			inputIndex: i,
			key:        key,
			label:      label,
			agent:      agent,
			blocked:    status == assessBlocked,
		}
		targets = append(targets, t)
	}

	// Raw burn scores among assessable non-blocked keys.
	scores := make([]float64, len(targets))
	var underIdx []int
	for i, t := range targets {
		if t.blocked {
			continue
		}
		u, ok := urgency(t.agent)
		if !ok {
			// No usable monthly signal: leave untouched (do not overwrite).
			targets[i].weight = -1
			continue
		}
		scores[i] = u // provisional; may be cleared if under-burners exist
		if underBurner(t.agent) {
			underIdx = append(underIdx, i)
		}
	}

	if len(underIdx) > 0 {
		// Under-burners take all weight; on-track keys get 0.
		for i := range scores {
			scores[i] = 0
		}
		for _, i := range underIdx {
			u, ok := urgency(targets[i].agent)
			if ok {
				scores[i] = u
			}
		}
	}
	// else: zero under-burners → keep urgency scores among keys with remaining

	weights := normalizeScoresToHundred(scores)
	for i := range targets {
		if targets[i].weight < 0 {
			continue // unassessable monthly signal: leave effective untouched
		}
		if targets[i].blocked {
			targets[i].weight = 0
		} else {
			targets[i].weight = weights[i]
		}
		effective[targets[i].inputIndex] = targets[i].weight
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

	// Fail-open spare ONLY when the pool has zero routable keys. Do not dilute
	// a single under-burner that already holds 100%.
	if len(active) == 0 {
		sort.SliceStable(targets, func(i, j int) bool {
			ui, _ := urgency(targets[i].agent)
			uj, _ := urgency(targets[j].agent)
			return ui > uj
		})

		rearmed := make([]int, 0, cfg.MinActive)
		for i := range targets {
			if len(active) >= cfg.MinActive {
				break
			}
			t := &targets[i]
			if t.weight < 0 {
				continue
			}
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
			rearmed = append(rearmed, i)
		}

		// Re-normalize re-armed (and any positive) managed weights to 100.
		if len(rearmed) > 0 {
			raw := make([]float64, len(targets))
			for i, t := range targets {
				if t.weight > 0 {
					raw[i] = t.weight
				}
			}
			normed := normalizeScoresToHundred(raw)
			for i := range targets {
				if targets[i].weight < 0 || targets[i].blocked {
					continue
				}
				targets[i].weight = normed[i]
				effective[targets[i].inputIndex] = targets[i].weight
			}
		}
	}

	var changes []Change
	for _, t := range targets {
		if t.weight < 0 {
			continue
		}
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
	// A readable monthly burn signal is enough, even when urgency is tiny:
	// the fail-open path can still bump the key then re-normalize to 100.
	_, ok := urgency(agent)
	return ok
}

type assessStatus int

const (
	assessOK assessStatus = iota
	assessBlocked
	assessSkip // unassessable / disabled: leave untouched
)

// assessKey applies hard blockers and assessability gates. Allocation among
// non-blocked keys is done in Compute (under-burner vs on-track + normalize).
//
// Rules, in priority order:
//  1. explicitly disabled                         → leave untouched
//  2. Bifrost reports the key as not healthy      → 0 (dead key)
//  3. rolling 5h at/above RollingEvictPercent     → 0 (blocked right now, ~5h)
//  4. weekly at/above WeeklyEvictPercent          → 0 (blocked until Monday)
//  5. monthly at the ceiling (100% consumed)      → 0 (nothing left to burn)
//  6. otherwise                                   → OK (score later)
func assessKey(cfg Config, key bifrost.Key, byLabel map[string]*quotas.Agent) assessStatus {
	if !routingEnabled(key) {
		return assessSkip
	}
	if key.Status != "success" {
		return assessBlocked
	}

	agent := byLabel[LabelFromEnv(key.Value.Ref)]
	if agent == nil || agent.Error != "" {
		return assessSkip
	}

	if rolling := agent.RollingPercent(); rolling >= 0 && rolling >= cfg.RollingEvictPercent {
		return assessBlocked
	}
	if weekly := agent.WeeklyPercent(); weekly >= 0 && weekly >= cfg.WeeklyEvictPercent {
		return assessBlocked
	}
	if monthly := agent.MonthlyPercent(); monthly >= 100 {
		return assessBlocked
	}
	return assessOK
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
