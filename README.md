# myrtille

`myrtille` orchestre des tests de charge [k6](https://k6.io) en trois phases :

1. **init** — met le service testé dans un état connu (création de données via des appels HTTP
   déclaratifs) et construit un dictionnaire d'état.
2. **run** — lance le scénario k6 en lui passant ce dictionnaire d'état, tout en scrapant
   périodiquement l'endpoint `/metrics` (format Prometheus) du service pour observer son
   comportement sous charge.
3. **report** — écrit un rapport (Markdown, JSON et/ou HTML) combinant le résumé de l'init, les
   résultats k6 (thresholds, percentiles, etc.) et les métriques collectées pendant le run. Le
   format HTML ajoute, pour chaque métrique scrapée, un graphique [Chart.js](https://www.chartjs.org)
   de son évolution dans le temps (et un graphique en barres par métrique k6 agrégée), avec
   tooltips au survol — la librairie est vendorée et embarquée dans le binaire (`go:embed`), donc
   le rapport reste autonome et se consulte hors-ligne, sans requête réseau ni CDN.

Un seul binaire CLI générique (`myrtille`), piloté par un fichier de config YAML par projet —
aucun code Go à écrire côté projet consommateur.

## Prérequis

- Go 1.27+
- Le binaire [`k6`](https://k6.io/docs/get-started/installation/) doit être présent sur le `PATH`.

## Installation

```sh
go install github.com/antobarth/myrtille/cmd/myrtille@latest
```

Ou en local :

```sh
go build -o bin/myrtille ./cmd/myrtille
```

## Utilisation

```sh
myrtille run --config myrtille.yaml
myrtille init --config myrtille.yaml   # exécute uniquement la phase d'init, pour debug
```

Le code de sortie de `myrtille run` reflète celui de k6 (0 = succès, 99 = thresholds échoués,
autre = erreur de script). Le rapport est toujours écrit, même en cas d'échec de l'init ou des
thresholds.

## Config (`myrtille.yaml`)

```yaml
name: my-service-load-test
ref: "JIRA-PROJ-45"          # optionnel, informatif, affiché dans le rapport

service:
  base_url: http://localhost:8080
  metrics:
    url: http://localhost:8080/metrics   # optionnel — omettre pour désactiver le scraping
    interval: 5s                          # fréquence de scrape pendant le run

init:
  steps:
    - name: create_users
      method: POST
      url: "{{.BaseURL}}/users"
      body: '{"name": "user-{{.Index}}"}'
      count: 20                # répète le step ; {{.Index}} disponible dans le template (0-based)
      extract:
        - path: "id"           # syntaxe gjson appliquée à la réponse JSON
          as: "user_ids"       # accumulé en tableau dans le dictionnaire d'état
    - name: list_products
      method: GET
      url: "{{.BaseURL}}/products"
      extract:
        - path: "#.id"         # syntaxe gjson pour extraire un champ de chaque élément d'un tableau
          as: "product_ids"

k6:
  script: ./scenario.js
  args: ["--vus", "10", "--duration", "30s"]   # passthrough vers `k6 run`, tel quel
  state_env: STATE_FILE        # nom de la variable d'env exposant le chemin du JSON d'état

report:
  output_dir: ./reports
  formats: ["markdown", "json", "html"]  # html ajoute des graphiques Chart.js interactifs
```

Chaque step d'init : la requête est abandonnée (et le run k6 annulé) au premier échec HTTP
(status >= 400) ou à toute erreur d'extraction — un service partiellement initialisé rendrait le
test de charge qui suit non fiable.

### Consommer l'état côté script k6

Le dictionnaire d'état est sérialisé en JSON et son chemin passé au subprocess k6 via la
variable d'environnement définie par `k6.state_env` (`STATE_FILE` par défaut). Pattern standard
côté script :

```js
const state = JSON.parse(open(__ENV.STATE_FILE));
const userId = state.user_ids[Math.floor(Math.random() * state.user_ids.length)];
```

## Exemple complet

Voir [`examples/demo-service`](examples/demo-service) : un service HTTP minimal
(`stubservice`), une config `myrtille.yaml` et un `scenario.js` qui l'exploitent, formant un
test de fumée end-to-end complet.

```sh
go build -o /tmp/stubservice ./examples/demo-service/stubservice
/tmp/stubservice &

go build -o bin/myrtille ./cmd/myrtille
bin/myrtille run --config examples/demo-service/myrtille.yaml
```

Le rapport est écrit dans `examples/demo-service/reports/<timestamp>/`.

Pour voir des graphiques réellement mouvementés dans le rapport HTML (plutôt que des courbes
plates ou strictement croissantes), voir [`examples/inventory-service`](examples/inventory-service) :
un stub dont les métriques dépendent de la charge (profondeur de file, latence, stock par SKU,
taux d'erreurs) et un `scenario.js` à montée/descente de charge (`stages`) pour les faire varier.

```sh
go build -o /tmp/inventory-stubservice ./examples/inventory-service/stubservice
/tmp/inventory-stubservice &

go build -o bin/myrtille ./cmd/myrtille
bin/myrtille run --config examples/inventory-service/myrtille.yaml
```
