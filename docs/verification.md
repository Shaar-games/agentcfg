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
interactif, les menus de navigation et d'action utilisent **Huh Select** :
flèches ↑/↓ pour choisir puis **Entrée pour valider**. Les cases à cocher
avec Espace et Entrée sont réservées aux sélections de plusieurs disponibilités
de harnesses, MCP ou skills. Ctrl+C annule. Aucune écriture sans confirmation.

Vérifier aussi les commandes directes `harness`/`harnesses`,
`skill`/`skills`, `mcp`/`mcps` et leur mode `--plain` (inventaire
sans écriture). Les cinq valeurs de `agentcfg completion` sont `bash`,
`zsh`, `fish`, `powershell` et `pwsh`; la sortie doit pouvoir être
chargée dans le shell cible sans lancer de MCP. Les commandes d'installation
sont dans [completion.md](completion.md).

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

Dans un environnement de test avec un home isolé et des fixtures de harnesses,
vérifier d'abord le flux `agentcfg init` :

1. `agentcfg init --plain` découvre les MCP et skills natifs et affiche un plan sans créer de fichier.
2. `agentcfg init` ouvre des listes Huh de sélection et demande une confirmation avant création des sources canoniques.
3. `agentcfg init` propose à la fin un plan de synchronisation native, puis une confirmation Huh distincte, sans exiger `--apply`.
4. `agentcfg init --yes` crée uniquement les sources canoniques, sans synchronisation native ; `init --plain` ne modifie aucun fichier.
5. Pour les scripts, `agentcfg sync --plain` prévisualise les changements, puis seul `agentcfg sync --apply --yes` autorise explicitement leur application.
6. Une annulation, un conflit d'entrée non gérée ou une modification concurrente ne doit produire aucun écrasement silencieux ; les changements autorisés sont sauvegardés et soumis aux vérifications CAS, l'adoption MCP TOML est refusée.

Lancer ensuite `agentcfg config`, puis vérifier que :

1. Le menu présente les sections harnesses, MCP, skills, plugins, diagnostics et synchronisation via Huh Select : Entrée ouvre la section choisie.
2. Les cases cochées apparaissent seulement dans les listes de disponibilités multiples ; annuler une édition ne modifie rien.
3. La prévisualisation de synchronisation reste en lecture seule avant confirmation et application.
4. Les détails, avertissements et conflits sont lisibles sur le terminal cible.

Les tests automatisés utilisent des processus et des serveurs factices. Ils ne
prouvent pas la compatibilité de bout en bout de chaque version de Claude Code,
Codex, Cursor, OpenCode, Gemini ou Antigravity. Les opérations de configuration
doivent être vérifiées avec les applications réelles avant d'affirmer une telle
compatibilité.
