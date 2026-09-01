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
2. `GET https://opencode.ai/zen/go/v1/usage` par clé → quotas **directement**
   depuis l'API OpenCode Go (les clés `OPENCODE_GO_API_KEY*` sont injectées
   dans l'environnement du sidecar)
3. Calcul local du budget (weekly % + projection mensuelle à sec) — **sans**
   dépendre du dashboard [opencode-usage-tracker](https://github.com/rjullien/opencode-usage-tracker)
4. `PUT /api/providers/opencode-go/keys/{id}` pour chaque changement

> **Découplage dashboard (revue Baptiste PR #166)** : le sidecar ne consomme
> PAS l'API du dashboard. Il lit l'API OpenCode Go directement et reproduit la
> math de budget localement. Le dashboard peut tomber sans impact sur le sidecar.

Le mapping clé Bifrost ↔ clé OpenCode se fait par la référence
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
| `BIFROST_URL` | `http://127.0.0.1:8080` | Base URL du gateway Bifrost (localhost quand sidecar dans le pod) |
| `INTERVAL` | `1h` | Durée entre deux cycles (`30m`, `45s`, …) |
| `WEEKLY_THRESHOLD` | `80` | % weekly au-delà duquel une clé sort de rotation (`0` désactive la règle) |
| `PINNED_KEYS` | *(vide)* | Clés à ne JAMAIS toucher, séparées par des virgules (nom ou id) |
| `DRY_RUN` | `false` | Log les changements sans les appliquer |
| `OPENCODE_GO_API_KEY*` | *(requis)* | Clés OpenCode Go à surveiller : `OPENCODE_GO_API_KEY` = Main, `OPENCODE_GO_API_KEY_A` = A, etc. |

## Déploiement

Sidecar du pod Bifrost (même réseau → `http://127.0.0.1:8080`) :

```yaml
containers:
  - name: bifrost-weights
    image: ghcr.io/rjullien/bifrost-weight-sidecar:main
    env:
      - name: BIFROST_URL
        value: http://127.0.0.1:8080
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