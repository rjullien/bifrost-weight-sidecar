// Command sidecar periodically reconciles Bifrost opencode-go key weights with
// OpenCode Go weekly and monthly quota positions.
package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rjullien/bifrost-weight-sidecar/internal/bifrost"
	"github.com/rjullien/bifrost-weight-sidecar/internal/engine"
	"github.com/rjullien/bifrost-weight-sidecar/internal/quotas"
)

type config struct {
	BifrostURL   string
	Interval     time.Duration
	CycleTimeout time.Duration
	RetryBackoff time.Duration
	Pinned       map[string]bool
	DryRun       bool
}

// maxRetryBackoff plafonne le backoff exponentiel. Bifrost v2 met ~35s à
// bootstrapper ; réessayer 15s → 30s → 60s → … (plafond 5 min) couvre ce
// démarrage sans laisser le sidecar KO pendant tout l'intervalle.
const maxRetryBackoff = 5 * time.Minute

type bifrostAPI interface {
	KeysContext(context.Context) ([]bifrost.Key, error)
	KeyContext(context.Context, string) (bifrost.Key, error)
	SetWeightContext(context.Context, bifrost.Key, float64) error
}

type quotaAPI interface {
	UsageContext(context.Context, quotas.Keys) []quotas.Agent
}

type applyResult struct {
	Succeeded int
	Failed    int
	Skipped   int
}

func loadConfig() (config, error) {
	bifrostURL, err := envURL("BIFROST_URL", "http://127.0.0.1:8080")
	if err != nil {
		return config{}, err
	}
	interval, err := envDuration("INTERVAL", time.Hour)
	if err != nil {
		return config{}, err
	}
	cycleTimeout, err := envDuration("CYCLE_TIMEOUT", 5*time.Minute)
	if err != nil {
		return config{}, err
	}
	retryBackoff, err := envDuration("RETRY_BACKOFF", 15*time.Second)
	if err != nil {
		return config{}, err
	}
	dryRun, err := envBool("DRY_RUN", false)
	if err != nil {
		return config{}, err
	}
	return config{
		BifrostURL:   bifrostURL,
		Interval:     interval,
		CycleTimeout: cycleTimeout,
		RetryBackoff: retryBackoff,
		Pinned:       envSetOr("PINNED_KEYS", nil),
		DryRun:       dryRun,
	}, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration invalide: %v", err)
	}
	log.Printf("sidecar: bifrost=%s interval=%s cycle_timeout=%s retry_backoff=%s pinned=%v dry_run=%v policy=cramer-monthly",
		urlForLog(cfg.BifrostURL), cfg.Interval, cfg.CycleTimeout, cfg.RetryBackoff, cfg.Pinned, cfg.DryRun)

	bf := bifrost.NewClient(cfg.BifrostURL, 5*time.Second)
	qu := quotas.NewClient(5 * time.Second)
	pol := engine.Config{Pinned: cfg.Pinned, MinActive: 2}

	root, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// backoff s'applique uniquement quand Bifrost est injoignable (souvent son
	// bootstrap au démarrage du pod). Il est remis à zéro dès qu'un cycle
	// atteint Bifrost, et l'attente reste interruptible par un signal d'arrêt.
	backoff := cfg.RetryBackoff
	for root.Err() == nil {
		started := time.Now()
		cycleCtx, cancel := context.WithTimeout(root, cfg.CycleTimeout)
		reachable := runCycle(cycleCtx, cfg, pol, bf, qu)
		cancel()
		log.Printf("cycle terminé en %s", time.Since(started).Round(time.Millisecond))

		wait := cfg.Interval
		if reachable {
			backoff = cfg.RetryBackoff
		} else {
			wait = backoff
			log.Printf("bifrost injoignable — prochain essai dans %s", backoff)
			if backoff < maxRetryBackoff {
				if backoff *= 2; backoff > maxRetryBackoff {
					backoff = maxRetryBackoff
				}
			}
		}

		timer := time.NewTimer(wait)
		select {
		case <-root.Done():
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
	log.Printf("arrêt demandé, sidecar terminé")
}

// runCycle reconciles one snapshot. It returns false only when Bifrost itself
// is unreachable, so the caller can retry quickly with a backoff instead of
// waiting the full interval. A degraded quota cycle still counts as reachable.
func runCycle(ctx context.Context, cfg config, pol engine.Config, bf bifrostAPI, qu quotaAPI) bool {
	keys, err := bf.KeysContext(ctx)
	if err != nil {
		log.Printf("cycle KO: %v", err)
		return false
	}

	quotaKeys := openCodeKeysFromEnv()
	agents := qu.UsageContext(ctx, quotaKeys)
	quotaErrors := 0
	if len(agents) == 0 {
		quotaErrors++
		log.Printf("cycle dégradé: aucune clé OpenCode dans l'environnement (OPENCODE_GO_API_KEY*)")
	}
	for _, agent := range agents {
		if agent.Error != "" {
			quotaErrors++
			log.Printf("quota %q KO: %s", agent.Label, agent.Error)
		}
	}

	// Always compute, even without quota agents: Bifrost health remains an
	// independent fail-closed signal for managed env-backed keys.
	changes := engine.Compute(pol, engine.Input{Keys: keys, Agents: agents})
	if len(changes) == 0 {
		logCycleOutcome(len(keys), quotaErrors, "aucun changement de poids")
		return true
	}

	if cfg.DryRun {
		for _, ch := range changes {
			log.Printf("[dry-run] %q: poids %.3f -> %.3f", ch.Key.Name, ch.From, ch.To)
		}
		logCycleOutcome(len(keys), quotaErrors, fmt.Sprintf("%d changement(s) simulé(s)", len(changes)))
		return true
	}

	// Recompute from a fresh full snapshot, then fetch each key once more just
	// before its PUT in applyChanges. This cannot replace server-side CAS, but it
	// minimizes the stale full-object update window.
	freshKeys, err := bf.KeysContext(ctx)
	if err != nil {
		log.Printf("cycle KO: relecture bifrost avant écriture: %v", err)
		return false
	}
	changes = engine.Compute(pol, engine.Input{Keys: freshKeys, Agents: agents})
	if len(changes) == 0 {
		logCycleOutcome(len(freshKeys), quotaErrors, "état actualisé, aucun changement de poids")
		return true
	}

	result, err := applyChanges(ctx, bf, changes)
	if err != nil {
		log.Printf("cycle dégradé: %v", err)
	}
	log.Printf("cycle application: %d réussie(s), %d échouée(s), %d ignorée(s)",
		result.Succeeded, result.Failed, result.Skipped)
	if quotaErrors > 0 || result.Failed > 0 || result.Skipped > 0 {
		log.Printf("cycle dégradé: %d erreur(s) quota", quotaErrors)
	}
	return true
}

// applyChanges applies increases first. If any increase fails, decreases are
// skipped to avoid shrinking the active pool without its replacement. Each key
// is refreshed immediately before PUT; weight and every policy input must still
// match the plan. All successful writes are verified by a final snapshot.
func applyChanges(ctx context.Context, bf bifrostAPI, changes []engine.Change) (applyResult, error) {
	ordered := append([]engine.Change(nil), changes...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return changePriority(ordered[i]) < changePriority(ordered[j])
	})

	var result applyResult
	activationFailed := false
	type expectedState struct {
		weight float64
		key    bifrost.Key
	}
	expected := make(map[string]expectedState)
	for _, change := range ordered {
		decrease := change.To < change.From && !engine.WeightsEqual(change.To, change.From)
		increase := change.To > change.From && !engine.WeightsEqual(change.To, change.From)
		if decrease && activationFailed {
			result.Skipped++
			log.Printf("mise à jour %q ignorée: activation précédente en échec", change.Key.Name)
			continue
		}

		current, err := bf.KeyContext(ctx, change.Key.ID)
		if err != nil {
			result.Failed++
			activationFailed = activationFailed || increase
			log.Printf("ERREUR relecture %q: %v", change.Key.Name, err)
			continue
		}
		if !sameDecisionInputs(current, change.Key) {
			result.Skipped++
			activationFailed = activationFailed || increase
			log.Printf("mise à jour %q ignorée: configuration décisionnelle modifiée en concurrence", change.Key.Name)
			continue
		}
		if !engine.WeightsEqual(current.Weight, change.From) {
			result.Skipped++
			activationFailed = activationFailed || increase
			log.Printf("mise à jour %q ignorée: poids concurrent %.3f, attendu %.3f",
				change.Key.Name, current.Weight, change.From)
			continue
		}
		if err := bf.SetWeightContext(ctx, current, change.To); err != nil {
			result.Failed++
			activationFailed = activationFailed || increase
			log.Printf("ERREUR %q: %v", change.Key.Name, err)
			continue
		}
		result.Succeeded++
		expected[change.Key.ID] = expectedState{weight: change.To, key: current}
		log.Printf("%q: poids %.3f -> %.3f", change.Key.Name, change.From, change.To)
	}

	if len(expected) == 0 {
		return result, nil
	}
	finalKeys, err := bf.KeysContext(ctx)
	if err != nil {
		result.Failed++
		return result, fmt.Errorf("vérification finale bifrost impossible: %w", err)
	}
	seen := make(map[string]bool, len(expected))
	for _, key := range finalKeys {
		want, ok := expected[key.ID]
		if !ok {
			continue
		}
		seen[key.ID] = true
		if !engine.WeightsEqual(key.Weight, want.weight) || !sameDecisionInputs(key, want.key) {
			result.Failed++
		}
	}
	for id := range expected {
		if !seen[id] {
			result.Failed++
		}
	}
	if result.Failed > 0 {
		return result, fmt.Errorf("état final bifrost différent du plan appliqué")
	}
	return result, nil
}

func sameDecisionInputs(current, planned bifrost.Key) bool {
	return current.ID == planned.ID &&
		current.Name == planned.Name &&
		current.Status == planned.Status &&
		current.Value.Ref == planned.Value.Ref &&
		current.Value.Type == planned.Value.Type &&
		routingEnabled(current) == routingEnabled(planned)
}

func routingEnabled(key bifrost.Key) bool {
	return key.Enabled == nil || *key.Enabled
}

func changePriority(change engine.Change) int {
	switch {
	case change.To > change.From && !engine.WeightsEqual(change.To, change.From):
		return 0
	case change.To < change.From && !engine.WeightsEqual(change.To, change.From):
		return 2
	default:
		return 1
	}
}

func logCycleOutcome(keyCount, quotaErrors int, message string) {
	if quotaErrors > 0 {
		log.Printf("cycle dégradé: %d clés, %d erreur(s) quota, %s", keyCount, quotaErrors, message)
		return
	}
	log.Printf("cycle OK: %d clés, %s", keyCount, message)
}

// openCodeKeysFromEnv discovers and trims subscription secrets.
func openCodeKeysFromEnv() quotas.Keys {
	keys := quotas.Keys{}
	seen := map[string]bool{}
	envs := os.Environ()
	sort.Strings(envs)
	for _, kv := range envs {
		name, value, ok := strings.Cut(kv, "=")
		value = strings.TrimSpace(value)
		if !ok || value == "" {
			continue
		}
		const prefix = "OPENCODE_GO_API_KEY"
		if name == prefix {
			keys["Main"] = value
			seen["Main"] = true
			continue
		}
		if strings.HasPrefix(name, prefix+"_") {
			label := strings.TrimPrefix(name, prefix+"_")
			if label != "" && !seen[label] {
				keys[label] = value
				seen[label] = true
			}
		}
	}
	return keys
}

func envURL(key, def string) (string, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		raw = def
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s: URL HTTP(S) absolue sans query ni fragment attendue", key)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func urlForLog(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "<URL invalide>"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func envBool(key string, def bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	value, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: booléen invalide", key)
	}
	return value, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: durée invalide: %w", key, v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s=%q: la durée doit être strictement positive", key, v)
	}
	return d, nil
}

func envSetOr(key string, def map[string]bool) map[string]bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	set := make(map[string]bool)
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			set[item] = true
		}
	}
	return set
}
