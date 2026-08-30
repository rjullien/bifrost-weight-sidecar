# Bifrost Weight Sidecar

Contrôleur de poids pour le provider `opencode-go` du gateway **Bifrost**.
Tourne en sidecar dans le pod Bifrost et rééquilibre dynamiquement les poids de
chargement des clés en fonction des quotas OpenCode Go.

**Phase 1 (actuelle)** : rééquilibrage piloté par les quotas, 1×/heure.
**Phase 2 (prévue)** : détection de clé dead par scan des logs Bifrost
(`/api/logs`), réaction immédiate sans attendre l'heure.

## Principe

À chaque cycle (toutes les `INTERVAL`) :

1. `GET /api/providers/opencode-go/keys` → poids actuels + statut de chaque clé
2. `GET <DASHBOARD_URL>/api/usage` → quotas par abonnement (dashboard
   [opencode-usage-tracker](https://github.com/rjullien/opencode-usage-tracker))
3. Calcul des poids cibles (règles ci-dessous)
4. `PUT /api/providers/opencode-go/keys/{id}` pour chaque changement

Le mapping clé Bifrost ↔ abonnement dashboard se fait par la référence
d'environnement (`env.OPENCODE_GO_API_KEY_A` → label `A`,
`env.OPENCODE_GO_API_KEY` → `Main`), la même règle que celle du dashboard.

## Règles (par ordre de priorité)

| # | Règle | Poids |
|---|-------|-------|
| 1 | Bifrost signale la clé non saine (`status != success`) | `0` |
| 2 | Consommation weekly ≥ `WEEKLY_THRESHOLD` | `0` |
| 3 | Quota mensuel projeté à sec (`dryDays > 0`) | `0` |
| 4 | Sinon (clé saine, budget OK) | `1` |

Une clé dont les quotas ne sont **pas évaluables** (absente du dashboard, ou
agent en erreur) est laissée **intacte** : jamais de décision sur données
incomplètes.

## Variables d'environnement

| Variable | Défaut | Description |
|----------|--------|-------------|
| `BIFROST_URL` | `http://localhost:8080` | Base URL du gateway Bifrost (localhost quand sidecar dans le pod) |
| `DASHBOARD_URL` | `http://opencode-dashboard.opencode-dashboard.svc.cluster.local:8080` | Base URL du dashboard quotas |
| `INTERVAL` | `1h` | Durée entre deux cycles (`30m`, `45s`, …) |
| `WEEKLY_THRESHOLD` | `80` | % weekly au-delà duquel une clé sort de rotation (`0` désactive la règle) |
| `PINNED_KEYS` | *(vide)* | Clés à ne JAMAIS toucher, séparées par des virgules (nom ou id) |
| `DRY_RUN` | `false` | Log les changements sans les appliquer |

## Déploiement

Sidecar du pod Bifrost (même réseau → `http://localhost:8080`) :

```yaml
containers:
  - name: bifrost-weights
    image: ghcr.io/rjullien/bifrost-weight-sidecar:main
    env:
      - name: BIFROST_URL
        value: http://localhost:8080
    resources:
      requests: { cpu: 5m, memory: 16Mi }
      limits:   { cpu: 100m, memory: 64Mi }
```

## Test local (dry-run)

```bash
go run ./cmd/sidecar
# avec BIFROST_URL et DASHBOARD_URL par défaut (cluster) en --dry-run :
DRY_RUN=true go run ./cmd/sidecar
```