# Phase 2 — Détection de clé dead (design)

Statut : **proposé** — implémentation après validation.
Objectif : sortir une clé `opencode-go` de rotation **immédiatement** quand elle
devient défaillante, sans attendre le cycle horaire de rebalancement (phase 1).

## Constat : pas besoin de scraper les logs bruts

Bifrost expose une **API d'agrégation** (`GET /api/logs/stats`) — celle utilisée
par l'onglet « Provider Usage » de l'UI. Vérifiée sur l'instance prod (v1.6.11) :

```
GET /api/logs/stats?period=24h
{
  "total_requests": 1284,
  "success_rate": 97.43,
  "user_facing_success_rate": 97.43,
  "user_facing_total_requests": 1286,
  "average_latency": 8517.17,
  "total_tokens": 235796403,
  "prompt_tokens": 235003443,
  "completion_tokens": 792960,
  "total_cost": 42.05
}
```

### Filtres supportés (query params, source : `handlers/logging.go`)

| Param | Usage |
|-------|-------|
| `providers` | `opencode-go` |
| `models` | liste de modèles |
| `status` | filtre sur le statut des requêtes (`error`, `success`, …) |
| `selected_key_ids` | **filtre par clé Bifrost** (id ou nom) |
| `period` | fenêtre relative type `24h`, `1h`, `10m` |
| `start_time` / `end_time` | RFC3339Nano explicite |
| `min_latency`, `max_latency`, `min_tokens`… | seuils complémentaires |

### Endpoints d'agrégation voisins

- `GET /api/logs/rankings` (+ `?by_dimension=`) — classements par clé/modèle/provider
- `GET /api/logs/histogram/{…}` — séries temporelles (latency, tokens, cost, throughput)
- `GET /api/logs/filterdata` — dimensions disponibles
- `GET /api/logs/dashboard` — agrégat du dashboard

### Piège observé

`/api/logs/stats?period=1h&status=error` renvoie `total_requests: 0` sur un
créneau sans erreur — **bon signe** (le filtre `status` est bien actif), mais il
faut valider la sémantique exacte du champ `status` des logs (valeurs possibles)
avec 2-3 échantillons d'erreurs réelles avant de coder le seuil.

## Architecture proposée

```
main
├── ticker horaire (ph1)      → rebalance quotas (existant)
└── watcher dead-key (ph2)    → réaction immédiate
        │ toutes les DEAD_POLL_INTERVAL (30-60 s)
        ├── GET /api/logs/stats?period=ERROR_WINDOW&providers=opencode-go&status=error   (volume global)
        ├── GET /api/providers/opencode-go/keys  (statuts + poids actuels)
        └── évaluation par clé → PUT weight 0 si dead
```

- **Nouveau package `internal/health`** : client stats Bifrost + évaluateur.
- **Indépendant du ticker ph1** : deux goroutines, partage du client HTTP et du
  validateur de payload `SetWeight`.
- **Fail-safe** : si `/api/logs/stats` ou `/keys` est injoignable → le watcher
  ne fait **rien** (jamais de décision sur données incomplètes — même règle que ph1).

## Critères de dead key (proposition à valider)

Une clé est déclarée **dead** si, sur la fenêtre `ERROR_WINDOW` :

1. `status` renvoyé par `/api/providers/opencode-go/keys` ≠ `success` (déjà géré ph1), **ou**
2. volume d'échecs enregistré par clé ≥ `ERROR_THRESHOLD` (ex. 3), **ou**
3. `success_rate` < `MIN_SUCCESS_RATE` (ex. 60 %) sur un volume ≥ `MIN_REQUESTS` (ex. 5) — évite de tuer une clé pour 1 seul échec transitoire.

Les erreurs « permanentes » (401/402/403 — `insufficient balance`/clé invalide)
et 429 répétés sont les cas cibles ; un TCP timeout isolé ne doit **pas** tuer
une clé (d'où le seuil en volume).

## Réintégration

Une clé dead repose en rotation via le **cycle horaire ph1** une fois que :

- ses quotas sont redevenus sains (weekly < seuil, monthly pas à sec), **et**
- la dernière fenêtre `ERROR_WINDOW` est **propre** (0 échec) — grace period par défaut : la fenêtre elle-même (10 min), paramétrable via `REENABLE_GRACE`.

Le `PINNED_KEYS` de ph1 s'applique aussi au watcher : une clé épinglée n'est
jamais touchée, même en erreur.

## Config (env, défauts)

| Variable | Défaut | Description |
|----------|--------|-------------|
| `DEAD_POLL_INTERVAL` | `30s` | Fréquence du scan stats |
| `ERROR_WINDOW` | `10m` | Fenêtre glissante d'observation |
| `ERROR_THRESHOLD` | `3` | Nb d'échecs par clé pour déclarer dead |
| `MIN_SUCCESS_RATE` | `60` | % succès minimal (si volume ≥ `MIN_REQUESTS`) |
| `MIN_REQUESTS` | `5` | Volume minimal pour appliquer la règle de taux |
| `REENABLE_GRACE` | `10m` | Fenêtre propre exigée avant réintégration |

## Validation

1. Tests httptest sur le format réel de `/api/logs/stats` (capturé ci-dessus).
2. Dry-run prolongé (2-3 cycles + scans) contre la prod sans appliquer.
3. Cartographie : reproduire les choix de `ResolvePeriod`/`status` du code Bifrost
   sur 2-3 échantillons d'erreurs réelles (via `/api/logs?status=error`) pour
   figer la sémantique avant le déploiement.