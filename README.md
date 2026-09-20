# Bifrost Weight Sidecar

Contrôleur de poids pour le provider `opencode-go` du gateway **Bifrost**.
Tourne en sidecar dans le pod Bifrost et rééquilibre dynamiquement les poids de
chargement des clés en fonction des quotas OpenCode Go.

**Phase 1 (actuelle)** : rééquilibrage piloté par les quotas, toutes les 10 min.
**Phase 2 (prévue)** : détection de clé dead par scan des logs Bifrost
(`/api/logs`), réaction immédiate sans attendre le cycle.

## Principe

À chaque cycle (toutes les `INTERVAL`, 10 min par défaut) :

1. `GET /api/providers/opencode-go/keys` → poids actuels + statut de chaque clé
2. `GET https://opencode.ai/zen/go/v1/usage` par clé, avec au plus **4 requêtes simultanées** → quotas **directement**
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

**Politique : « cramer le monthly, garde-fous weekly + rolling, secours ≥ 2 »** —
le quota mensuel non consommé avant le reset anniversaire est **perdu**, donc le
poids d'une clé reflète l'urgence de consommation :

| # | Règle | Poids |
|---|-------|-------|
| 1 | Bifrost signale la clé non saine (`status != success`) | `0` |
| 2 | Rolling 5h ≥ `ROLLING_EVICT_PERCENT` (bloqueur, brut) | `0` |
| 3 | Weekly ≥ `WEEKLY_EVICT_PERCENT` (bloqueur, brut) | `0` |
| 4 | Monthly à **100 %** (plafond strict, plus rien à cramer) | `0` |
| 5 | Sinon | **urgence** = monthly restant (%) ÷ jours restants |

**Bloqueurs (rolling & weekly)** : gradés sur la **consommation brute** à un seuil
proche du plafond (**99 %** par défaut, chacun réglable). À ce niveau la clé est
déjà en train d'échouer — inutile de lui envoyer du trafic. Pas de projection :

- **Rolling 5h** — fenêtre glissante qui se vide seule, la clé **réintègre la
  rotation d'elle-même** dès qu'elle repasse sous le seuil.
- **Weekly** — bloqueur jusqu'au reset du lundi ; même logique de retour
  automatique une fois la fenêtre réinitialisée.

**Monthly** : évincé **uniquement à 100 %**, jamais sur une projection. Le quota
mensuel non consommé est perdu au reset (*use-it-or-lose-it*) : une clé encore
sous 100 % garde du quota à cramer, donc elle **reste en rotation** (avec une
urgence plus élevée) plutôt que de gaspiller.

Une clé bloquée (rolling, weekly, monthly à 100 % ou morte côté Bifrost) n'est
**jamais** réarmée par le filet de secours : elle échouerait. Une clé bloquée uniquement sur le
rolling n'est **jamais** réarmée par le filet de secours (elle échouerait).

**Urgence** : plus le monthly restant expire vite, plus le poids est élevé (la
clé « crame » son quota avant qu'il soit perdu). Bifrost route en proportion
des poids (float acceptés — vérifié runtime).

**Secours** : le contrôleur essaie de garder au moins **2 clés utilisables** avec
un poids non nul — la première à **1**, les suivantes à **0.5** (réserve
vivante, peu de trafic mais disponible). Les clés mortes côté Bifrost
(`status != success`) ou sans quota mensuel restant ne sont **jamais**
réarmées : si moins de deux clés peuvent réellement servir, le contrôleur
préfère laisser le pool dégradé plutôt que leur envoyer du trafic voué à
échouer.

Une clé dont les quotas ne sont **pas évaluables** (absente de l'API, ou agent
en erreur) est laissée **intacte** : jamais de décision sur données incomplètes.

## Variables d'environnement

| Variable | Défaut | Description |
|----------|--------|-------------|
| `BIFROST_URL` | `http://127.0.0.1:8080` | URL HTTP(S) absolue du gateway Bifrost, sans query ni fragment (localhost IPv4 quand sidecar dans le pod) |
| `INTERVAL` | `10m` | Durée strictement positive entre la fin d’un cycle et le suivant (`10m`, `30m`, `45s`, …) |
| `ROLLING_EVICT_PERCENT` | `99` | Seuil (%) du rolling 5h au-delà duquel la clé sort de rotation. Entier dans `[1,100]` |
| `WEEKLY_EVICT_PERCENT` | `99` | Seuil (%) du weekly au-delà duquel la clé sort de rotation. Entier dans `[1,100]` |
| `PINNED_KEYS` | *(vide)* | Clés à ne JAMAIS toucher, séparées par des virgules (nom ou id) |
| `DRY_RUN` | `false` | Log les changements sans les appliquer |
| `OPENCODE_GO_API_KEY*` | *(requis)* | Clés OpenCode Go à surveiller : `OPENCODE_GO_API_KEY` = Main, `OPENCODE_GO_API_KEY_A` = A, etc. |

## Déploiement

Sidecar du pod Bifrost (même réseau → `http://127.0.0.1:8080`) :

```yaml
containers:
  - name: bifrost-weights
    image: ghcr.io/rjullien/bifrost-weight-sidecar:main
    securityContext:
      runAsNonRoot: true
      runAsUser: 65532
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: ["ALL"]
    env:
      - name: BIFROST_URL
        value: http://127.0.0.1:8080
    resources:
      requests: { cpu: 5m, memory: 16Mi }
      limits:   { cpu: 100m, memory: 64Mi }
```

## Test local (dry-run)

```bash
# utilise BIFROST_URL=http://127.0.0.1:8080 par défaut
DRY_RUN=true go run ./cmd/sidecar
```