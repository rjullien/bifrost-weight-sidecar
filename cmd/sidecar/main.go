// Command sidecar runs the Bifrost weight controller: every interval it reads
// the opencode-go key weights from Bifrost and the quota positions directly
// from the OpenCode Go usage API (keys injected in its own environment),
// computes the target weights, and applies the changes through the Bifrost
// management API.
//
// The controller is self-contained: it does NOT depend on the
// opencode-usage-tracker dashboard (review Baptiste, PR #166). The pace math
// (weekly/monthly budget, dry days) is reproduced in internal/quotas.
package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rjullien/bifrost-weight-sidecar/internal/bifrost"
	"github.com/rjullien/bifrost-weight-sidecar/internal/engine"
	"github.com/rjullien/bifrost-weight-sidecar/internal/quotas"
)

type config struct {
	BifrostURL   string
	Interval     time.Duration
	RetryBackoff time.Duration
	Pinned       map[string]bool
	DryRun       bool
}

const (
	// retryBackoffStart : délai initial après un échec de connexion à Bifrost.
	retryBackoffStart = 15 * time.Second
	// maxRetryBackoff : plafond du backoff exponentiel (bootstrap bifrost v2 ~35s,
	// 3 essais ≈ 15+30+60s → couvre largement).
	maxRetryBackoff = 5 * time.Minute
)

func loadConfig() (config, error) {
	bifrostURL, err := envURL("BIFROST_URL", "http://127.0.0.1:8080")
	if err != nil {
		return config{}, err
	}
	interval, err := envDuration("INTERVAL", time.Hour)
	if err != nil {
		return config{}, err
	}
	retryBackoff := retryBackoffStart
	if v, err := envDuration("RETRY_BACKOFF", retryBackoffStart); err == nil {
		retryBackoff = v
	} else {
		return config{}, err
	}
	return config{
		BifrostURL:   bifrostURL,
		Interval:     interval,
		RetryBackoff: retryBackoff,
		Pinned:       envSetOr("PINNED_KEYS", nil),
		DryRun:       envBoolOr("DRY_RUN", false),
	}, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration invalide: %v", err)
	}
	log.Printf("sidecar: bifrost=%s interval=%s pinned=%v dry_run=%v policy=cramer-monthly (weekly=bloqueur, secours >= 2)",
		urlForLog(cfg.BifrostURL), cfg.Interval, cfg.Pinned, cfg.DryRun)

	bf := bifrost.NewClient(cfg.BifrostURL, 5*time.Second)
	qu := quotas.NewClient(5 * time.Second)
	pol := engine.Config{Pinned: cfg.Pinned, MinActive: 2}

	run := func() bool {
		keys, err := bf.Keys()
		if err != nil {
			log.Printf("cycle KO: %v", err)
			return false // bifrost injoignable → réessayer vite (backoff)
		}

		agents := qu.Usage(openCodeKeysFromEnv())
		if len(agents) == 0 {
			log.Printf("cycle KO: aucune clé OpenCode dans l'environnement (OPENCODE_GO_API_KEY*)")
			return true
		}

		quotaErrors := 0
		for _, agent := range agents {
			if agent.Error != "" {
				quotaErrors++
				log.Printf("quota %s KO: %s", agent.Label, agent.Error)
			}
		}

		changes := engine.Compute(pol, engine.Input{Keys: keys, Agents: agents})
		if len(changes) == 0 {
			if quotaErrors > 0 {
				log.Printf("cycle dégradé: %d clés, %d quota(s) indisponible(s), aucun changement de poids", len(keys), quotaErrors)
			} else {
				log.Printf("cycle OK: %d clés, aucun changement de poids", len(keys))
			}
			return true
		}

		// Refresh the Bifrost snapshot after quota collection. SetWeight must
		// send a full key object, so using fresh models and secret references
		// narrows the window in which a concurrent management change is lost.
		if !cfg.DryRun {
			freshKeys, err := bf.Keys()
			if err != nil {
				log.Printf("cycle KO: relecture bifrost avant écriture: %v", err)
				return false
			}
			changes = engine.Compute(pol, engine.Input{Keys: freshKeys, Agents: agents})
			if len(changes) == 0 {
				if quotaErrors > 0 {
					log.Printf("cycle dégradé: état Bifrost actualisé, %d quota(s) indisponible(s), aucun changement de poids", quotaErrors)
				} else {
					log.Printf("cycle OK: état Bifrost actualisé, aucun changement de poids")
				}
				return true
			}
		}

		failed := 0
		for _, ch := range changes {
			if cfg.DryRun {
				log.Printf("[dry-run] %s: poids %.3f -> %.3f", ch.Key.Name, ch.From, ch.To)
				continue
			}
			if err := bf.SetWeight(ch.Key, ch.To); err != nil {
				failed++
				log.Printf("ERREUR %s: %v", ch.Key.Name, err)
				continue
			}
			log.Printf("%s: poids %.3f -> %.3f", ch.Key.Name, ch.From, ch.To)
		}
		if failed > 0 || quotaErrors > 0 {
			log.Printf("cycle dégradé: %d erreur(s) quota, %d mise(s) à jour échouée(s)", quotaErrors, failed)
		}
		return true
	}

	// Boucle principale : après un échec de connexion à Bifrost (ex: bootstrap
	// encore en cours au démarrage, ~35s sur bifrost v2), réessayer avec un
	// backoff exponentiel au lieu de dormir l'intervalle complet. Sans ça le
	// sidecar reste KO pendant 1h après un démarrage raté.
	backoff := cfg.RetryBackoff
	for {
		if run() {
			time.Sleep(cfg.Interval)
			continue
		}
		log.Printf("bifrost injoignable — prochain essai dans %s", backoff)
		time.Sleep(backoff)
		if backoff < maxRetryBackoff {
			backoff *= 2
			if backoff > maxRetryBackoff {
				backoff = maxRetryBackoff
			}
		}
	}
}

// openCodeKeysFromEnv discovers the OpenCode Go subscription keys from the
// environment, mirroring the dashboard: OPENCODE_GO_API_KEY is "Main",
// OPENCODE_GO_API_KEY_<SUFFIXE> is "<SUFFIXE>". Empty values are skipped.
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

// -- helpers env -- //

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

func envBoolOr(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
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
