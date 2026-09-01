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
	"log"
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
	BifrostURL      string
	Interval        time.Duration
	WeeklyThreshold int
	Pinned          map[string]bool
	DryRun          bool
}

func loadConfig() config {
	return config{
		BifrostURL:      envOr("BIFROST_URL", "http://127.0.0.1:8080"),
		Interval:        envDurationOr("INTERVAL", time.Hour),
		WeeklyThreshold: envIntOr("WEEKLY_THRESHOLD", 80),
		Pinned:          envSetOr("PINNED_KEYS", nil),
		DryRun:          envBoolOr("DRY_RUN", false),
	}
}

func main() {
	cfg := loadConfig()
	log.Printf("sidecar: bifrost=%s interval=%s weekly_threshold=%d%% pinned=%v dry_run=%v",
		cfg.BifrostURL, cfg.Interval, cfg.WeeklyThreshold, cfg.Pinned, cfg.DryRun)

	bf := bifrost.NewClient(cfg.BifrostURL, 5*time.Second)
	qu := quotas.NewClient(5 * time.Second)
	pol := engine.Config{WeeklyThreshold: cfg.WeeklyThreshold, Pinned: cfg.Pinned}

	run := func() {
		keys, err := bf.Keys()
		if err != nil {
			log.Printf("cycle KO: %v", err)
			return
		}

		agents := qu.Usage(openCodeKeysFromEnv())
		if len(agents) == 0 {
			log.Printf("cycle KO: aucune clé OpenCode dans l'environnement (OPENCODE_GO_API_KEY*)")
			return
		}

		changes := engine.Compute(pol, engine.Input{Keys: keys, Agents: agents})
		if len(changes) == 0 {
			log.Printf("cycle OK: %d clés, aucun changement de poids", len(keys))
			return
		}

		for _, ch := range changes {
			if cfg.DryRun {
				log.Printf("[dry-run] %s: poids %.0f -> %.0f", ch.Key.Name, ch.From, ch.To)
				continue
			}
			if err := bf.SetWeight(ch.Key, ch.To); err != nil {
				log.Printf("ERREUR %s: %v", ch.Key.Name, err)
				continue
			}
			log.Printf("%s: poids %.0f -> %.0f", ch.Key.Name, ch.From, ch.To)
		}
	}

	run() // première passe immédiate, puis ticker
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for range ticker.C {
		run()
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
			if !seen[label] {
				keys[label] = value
				seen[label] = true
			}
		}
	}
	return keys
}

// -- helpers env -- //

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBoolOr(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDurationOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
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
