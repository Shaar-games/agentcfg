# Vérification d'agentcfg

Ce document décrit les contrôles reproductibles du projet `agentcfg`. Le code
local, les artefacts de release et les applications clientes réelles ont des
niveaux de validation distincts. Un test local réussi ne prouve pas qu'une
release est publiée ou que son workflow GitHub Actions a réussi.

## Tests du dépôt

Depuis la racine du projet, avec Go 1.26 via mise :

```sh
mise exec -- go vet ./...
mise exec -- go test -count=1 -timeout=180s ./...
mise exec -- go test -race -count=1 -timeout=180s ./...
```

Les tests couvrent la configuration canonique, les plugins et skills, les
adaptateurs des harnesses, les changements natifs conservateurs, le broker,
les transports MCP et l'authentification. Les fixtures utilisent des répertoires
temporaires : elles ne doivent pas écrire dans les configurations des harnesses
de la machine de développement.

Les tests de la CLI vérifient notamment que `agentcfg` sans argument affiche
l'aide et que `agentcfg config` ouvre la configuration Huh. Sur un terminal
interactif, les listes de sélection des harnesses et des skills utilisent les
mêmes commandes : flèches ↑/↓, Espace pour cocher ou décocher, Entrée pour
valider et Ctrl+C pour annuler. La configuration n'est enregistrée qu'après
validation du formulaire concerné.

## Installateurs et binaires

```sh
mise exec -- go test -count=1 -timeout=180s -v ./internal/installtest
```

Les tests d'installateurs utilisent des archives servies localement et un
manifeste `SHA256SUMS` de test. Ils contrôlent le rejet d'un hash invalide,
l'installation d'un exécutable `agentcfg`, son aide sans argument et la
réinstallation dans un répertoire temporaire. PowerShell est testé sur Windows ;
Bash est testé sur Unix et, si Git Bash est disponible, sur Windows.

`.github/workflows/check.yml` exécute `go vet`, `go test`, un build CLI et un
smoke test de l'aide sur Windows, Ubuntu et macOS. Le détecteur de courses est
également exécuté sur Linux. `.github/workflows/release.yml` vérifie le code,
construit six binaires sans CGO (Linux, macOS et Windows × AMD64/ARM64), publie
les archives `agentcfg_*` et leur `SHA256SUMS`, puis teste l'installation des
archives publiées sous Windows, Linux et macOS.

Les builds croisés ARM64 établissent que la compilation réussit ; ils ne
remplacent pas un test sur une machine ARM64. Les scripts conservent le binaire
installé si le téléchargement, la vérification ou la préparation échoue.

## Vérification manuelle de Huh

Dans un environnement de test avec un home isolé, initialiser les sources,
lancer `agentcfg config`, puis vérifier que :

1. Le menu présente les sections harnesses, MCP, skills, plugins, diagnostics et synchronisation.
2. Les choix cochés sont visibles dans les listes multi-sélection, et annuler une édition ne modifie rien.
3. La prévisualisation de synchronisation reste en lecture seule avant confirmation et application.
4. Les détails, avertissements et conflits sont lisibles sur le terminal cible.

Les tests automatisés utilisent des processus et des serveurs factices. Ils ne
prouvent pas la compatibilité de bout en bout de chaque version de Claude Code,
Codex, Cursor, OpenCode, Gemini ou Antigravity. Les opérations de configuration
doivent être vérifiées avec les applications réelles avant d'affirmer une telle
compatibilité.
