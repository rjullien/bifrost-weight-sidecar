// Package engine turns Bifrost key state and OpenCode Go quota positions into
// weight changes.
//
// Weight policy — "cramer le monthly, garde-fou weekly, secours ≥ 2":
//
//   - Monthly quota is use-it-or-lose-it. A healthy key's weight reflects the
//     remaining monthly quota divided by days until reset.
//   - Weekly and monthly projected exhaustion remove a key from normal routing.
//   - The fallback may re-arm a key that is only projected to exhaust, but
//     never one already at a weekly/monthly ceiling or unhealthy in Bifrost.
//   - MinActive counts distinct enabled healthy subscriptions, including pinned
//     and untouched healthy keys; duplicate Bifrost entries sharing one env
//     ref count once.
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
		weight := targetWeight(key, byLabel)
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
			if t.weight > 0 || active[identity] || !fallbackEligible(t.key, t.agent) {
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
// A projected wall may be crossed to preserve availability; an already reached
// weekly/monthly ceiling and unhealthy Bifrost status are hard stops.
func fallbackEligible(key bifrost.Key, agent *quotas.Agent) bool {
	if key.Status != "success" || !routingEnabled(key) || agent == nil || agent.Error != "" {
		return false
	}
	if agent.WeeklyPercent() >= 100 || agent.MonthlyPercent() >= 100 {
		return false
	}
	u, ok := urgency(agent)
	return ok && u > 0
}

// targetWeight returns a non-negative target, or -1 when the state is not
// assessable. It is called only for managed env references.
func targetWeight(key bifrost.Key, byLabel map[string]*quotas.Agent) float64 {
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

	weeklyPercent := agent.WeeklyPercent()
	monthlyPercent := agent.MonthlyPercent()
	weeklyDry := agent.WeeklyDryDays()
	monthlyDry := agent.MonthlyDryDays()
	if weeklyPercent < 0 || weeklyPercent > 100 || monthlyPercent < 0 || monthlyPercent > 100 ||
		weeklyDry < 0 || monthlyDry < 0 {
		return -1
	}

	if weeklyDry > 0 || monthlyDry > 0 {
		return 0
	}

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
