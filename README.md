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
3. Calcul local des poids (burn-to-100 % monthly pour J−1, bloqueurs weekly/rolling bruts) — **sans**
   dépendre du dashboard [opencode-usage-tracker](https://github.com/rjullien/opencode-usage-tracker)
4. Relecture complète de Bifrost, puis relecture de chaque clé juste avant son
   `PUT /api/providers/opencode-go/keys/{id}`. Les augmentations sont appliquées
   avant les baisses ; si une activation échoue, les baisses restantes sont
   annulées. Un dernier snapshot vérifie les poids effectivement persistés.

> **Découplage dashboard (revue Baptiste PR #166)** : le sidecar ne consomme
> PAS l'API du dashboard. Il lit l'API OpenCode Go directement et reproduit la
> math de budget localement. Le dashboard peut tomber sans impact sur le sidecar.

Le mapping clé Bifrost ↔ clé OpenCode se fait par la référence
d'environnement (`env.OPENCODE_GO_API_KEY_A` → label `A`,
`env.OPENCODE_GO_API_KEY` → `Main`), la même règle que celle du dashboard.

## Règles (par ordre de priorité)

**Politique : « cramer le monthly à 100 % pour J−1 (reset − lead), garde-fous,
secours fail-open »** — le quota mensuel non consommé avant le reset
anniversaire est **perdu**. L'algo vise à **finir** chaque clé évaluable
**un jour complet avant** ce reset (mur de burn J−1 = `resetsAt − MONTHLY_BURN_LEAD`,
défaut `24h`). Il pousse le trafic vers les clés qui **n'atteindront pas 100 %**
à leur rythme actuel avant ce mur (`MonthlyDryDays == 0`), et normalise les
poids actifs pour qu'ils somment à **100** :

| # | Règle | Poids |
|---|-------|-------|
| 1 | Bifrost signale la clé non saine (`status != success`) | `0` |
| 2 | Rolling 5h ≥ `ROLLING_EVICT_PERCENT` (bloqueur, brut) | `0` |
| 3 | Weekly ≥ `WEEKLY_EVICT_PERCENT` (bloqueur, brut) | `0` |
| 4 | Monthly à **100 %** (plafond strict, plus rien à cramer) | `0` |
| 5a | ≥ 1 **sous-brûleur** (`DryDays == 0` vs mur J−1, monthly restant > 0) | **tout** le poids sur ces clés (1 → `100` ; plusieurs → split par urgence puis normalize Σ=`100`) ; les clés *on track* (`DryDays > 0`) → `0` |
| 5b | 0 sous-brûleur (toutes on track pour J−1) | urgence parmi les non-bloquées avec restant > 0, normalize Σ=`100` ; sinon tout à `0` |

`enabled: false` / pinned / non évaluable → **intacts** (`-1`), comme avant.
Les non évaluables restent dans le pool fail-open (comptage MinActive) s'ils
sont déjà actifs ; les pinned ne sont **jamais** écrasés.

**Bloqueurs (rolling & weekly)** : gradés sur la **consommation brute** à un seuil
proche du plafond (**99 %** par défaut, chacun réglable). À ce niveau la clé est
déjà en train d'échouer — inutile de lui envoyer du trafic. Pas de projection :

- **Rolling 5h** — fenêtre glissante qui se vide seule, la clé **réintègre la
  rotation d'elle-même** dès qu'elle repasse sous le seuil.
- **Weekly** — bloqueur jusqu'au reset du lundi ; même logique de retour
  automatique une fois la fenêtre réinitialisée.

**Monthly** : évincé **uniquement à 100 %**, jamais sur une projection. Le quota
mensuel non consommé est perdu au reset (*use-it-or-lose-it*).

**Sous-brûleurs vs on track** (`MonthlyDryDays`, calculé dans `internal/quotas`
contre le mur J−1 — pas contre `resetsAt`) :

- `DryDays > 0` — projeté d'atteindre le plafond **avant le mur**
  (`resetsAt − MONTHLY_BURN_LEAD`) → n'a **pas** besoin de trafic prioritaire.
- `DryDays == 0` — ne touchera **pas** 100 % d'ici J−1 au rythme actuel →
  reçoit le trafic (winner-take-all / split). Cas type : N à 98 % avec ~1,1 j
  avant reset — « on track » vs anniversaire (DryDays≈0,5) mais sous-brûleur
  vs J−1 → doit prendre (presque) tout le poids.

**Urgence** : `monthly restant (%) ÷ jours restants jusqu'au reset`. Sert
uniquement à répartir *entre* sous-brûleurs (ou en fallback 5b). Les poids
appliqués sont des **pourcentages** (Σ = 100), plus les anciennes magnitudes
brutes type `1.939` / `5.697`.

**Secours (MinActive)** : fail-open **uniquement** si le pool géré aurait
**zéro** clé routable (réarme des spares éligibles à petits poids, puis
re-normalize à 100). Le secours **ne dilue plus** un sous-brûleur unique qui
doit recevoir `100` — plus de conflit winner-take-all vs « toujours ≥ 2 ».

Une clé bloquée (rolling ou weekly au seuil, monthly à 100 %, ou morte côté
Bifrost) n'est **jamais** réarmée : elle échouerait.

**Exemple (prod-like, mur J−1)** : N ~98 % / ~1,1 j avant reset (sous-brûleur
vs J−1) ; R/A confortables on track pour le mur. → N à `100` ; R/A à `0`.
Sans le lead de 24 h, N aurait partagé ~21 % en fallback urgence.

Une clé dont les quotas ne sont **pas évaluables** (absente de l'API, agent en
erreur, ou signal monthly inutilisable) est laissée **intacte** : jamais de
décision de quota sur des données incomplètes. Le statut Bifrost reste un signal
indépendant : une clé non saine est mise à `0`, même si aucun quota OpenCode
n'est disponible. Une fenêtre rolling absente est ignorée (pas d'éviction) tant
que le monthly reste évaluable.

## Variables d'environnement

| Variable | Défaut | Description |
|----------|--------|-------------|
| `BIFROST_URL` | `http://127.0.0.1:8080` | URL HTTP(S) absolue du gateway Bifrost, sans query ni fragment (localhost IPv4 quand sidecar dans le pod) |
| `INTERVAL` | `10m` | Durée strictement positive entre la fin d’un cycle et le suivant (`10m`, `30m`, `45s`, …) |
| `CYCLE_TIMEOUT` | `5m` | Durée maximale strictement positive d'un cycle complet ; annule aussi les requêtes HTTP en cours |
| `RETRY_BACKOFF` | `15s` | Délai initial strictement positif avant réessai quand Bifrost est injoignable ; double à chaque échec jusqu'à 5 min |
| `ROLLING_EVICT_PERCENT` | `99` | Seuil (%) du rolling 5h au-delà duquel la clé sort de rotation. Entier dans `[1,100]` |
| `WEEKLY_EVICT_PERCENT` | `99` | Seuil (%) du weekly au-delà duquel la clé sort de rotation. Entier dans `[1,100]` |
| `MONTHLY_BURN_LEAD` | `24h` | Décalage du mur de burn mensuel avant `resetsAt` (J−1). `DryDays` / sous-brûleurs sont calculés contre `resetsAt − lead`. `0` = mur aligné sur le reset (ancien comportement). Durée ≥ 0 (`24h`, `12h`, `0`, …) |
| `PINNED_KEYS` | *(vide)* | Clés à ne JAMAIS toucher, séparées par des virgules (nom ou id) |
| `DRY_RUN` | `false` | Booléen strict (`true`/`false`, formes acceptées par Go) : log les changements sans les appliquer |
| `OPENCODE_GO_API_KEY*` | *(requis)* | Clés OpenCode Go à surveiller : `OPENCODE_GO_API_KEY` = Main, `OPENCODE_GO_API_KEY_A` = A, etc. |

Le premier cycle démarre immédiatement. `SIGTERM` et `SIGINT` annulent le cycle
ou l'attente en cours, puis arrêtent proprement le processus.

Quand Bifrost est injoignable (souvent son bootstrap au démarrage du pod, ~35 s
sur Bifrost v2), le sidecar réessaie avec un backoff exponentiel `RETRY_BACKOFF`
→ ×2 → … plafonné à 5 min, au lieu d'attendre l'`INTERVAL` complet. Le backoff
est remis à zéro dès qu'un cycle atteint Bifrost, et cette attente reste elle
aussi interruptible par un signal d'arrêt.

## CI et publication

Les pull requests exécutent les tests, `go vet`, `govulncheck` et un build
Docker multi-architecture sans permission d'écriture sur GHCR. La publication
est réservée aux pushes sur `main`, aux tags `v*`, aux déclenchements manuels et,
en fallback, à la fermeture d'une PR effectivement mergée. Ce fallback ne
checkout jamais la branche de la PR : il reconstruit uniquement `main`.

Les GitHub Actions et l'image Go du builder sont épinglées par SHA/digest. Les
images publiées pour `linux/amd64` et `linux/arm64` embarquent une provenance et
un SBOM OCI. Le job de publication est le seul à disposer de `packages: write`.

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