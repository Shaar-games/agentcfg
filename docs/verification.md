# Vérification d'agentcfg

Ce document décrit les contrôles reproductibles du projet `agentcfg`. Le code
local, les artefacts de release et les applications clientes réelles ont des
niveaux de validation distincts. Un test local réussi ne prouve pas qu'une
release est publiée ou que son workflow GitHub Actions a réussi.

## Tests du dépôt

Depuis la racine du projet, avec Go 1.26 via mise :

```sh
mise exec -- go vet ./...
mise exec -- go test -count=1 -timeout=300s ./...
mise exec -- go test -shuffle=on -count=1 -timeout=300s ./...
mise exec -- go test -race -count=1 -timeout=180s ./...
```

Les tests couvrent la configuration canonique, les plugins et skills, les
adaptateurs des harnesses, les changements natifs conservateurs, le broker,
les transports MCP et l'authentification. Les fixtures utilisent des répertoires
temporaires : elles ne doivent pas écrire dans les configurations des harnesses
de la machine de développement.

Les régressions de liens utilisent huit emplacements natifs de skills et leurs
56 paires orientées : liens absolus et relatifs sur Unix/Windows, plus jonctions
sur Windows, soit 168 parcours init → sync sans changement → restore sur Windows.
Des tests complémentaires couvrent les chaînes, les cibles hors du home, les
assets et SKILL.md liés, les fichiers MCP partagés, le TOML Codex, les parents
OpenCode/JSONC et le home lui-même lié. Les liens cassés et cycles sont refusés
sans modifier les sources. Les tests Windows nécessitant des symlinks signalent
un skip si le système ne permet pas leur création ; les jonctions sont testées
séparément sans ce privilège.

Les plugins combinent des liens sur le répertoire canonique, le catalogue,
le package, les manifestes, les composants MCP, les commandes et les skills.
Les tests d'édition vérifient que les liens sources restent présents et qu'une
cible changée depuis l'aperçu est refusée. Les tests de récupération simulent
une copie interrompue, un échec de synchronisation, des fichiers de staging
étrangers et un échec tardif de restauration suivi d'une nouvelle tentative.

Les tests de la CLI vérifient notamment que `agentcfg` sans argument affiche
l'aide et que `agentcfg config` ouvre la configuration Huh. Sur un terminal
interactif, les menus de navigation et d'action utilisent **Huh Select** :
flèches ↑/↓ pour choisir puis **Entrée pour valider**. Les cases à cocher
avec Espace et Entrée sont réservées aux sélections de plusieurs disponibilités
de harnesses, MCP ou skills. Ctrl+C annule. Aucune écriture sans confirmation.

Tous les formulaires passent par le même exécuteur `tui.RunForm`. Les listes
utilisent leur hauteur naturelle, recalculée au redimensionnement : aucune
hauteur fixe ne transforme une liste de navigation en viewport défilant.
Si la liste dépasse physiquement le terminal, Huh utilise une liste complète
numérotée plutôt que de masquer les premières options. Le mode accessible
reste disponible explicitement et n'émet pas d'ANSI ; `NO_COLOR` désactive
les couleurs des formulaires natifs sans supprimer leur navigation clavier.
Les statuts sont alignés par largeur visuelle, pas par longueur en octets.
Les formulaires natifs restent dans le terminal, sous la commande lancée :
aucun plein écran ni écran alternatif. Entrée et Ctrl+C nettoient uniquement
la zone du formulaire et ramènent le curseur au début de cette zone, sans
empiler les anciens menus ni effacer l'historique de l'utilisateur.

Les confirmations de mutation utilisent `Are you sure?`, `Yes!` et `No.`, avec
`No.` par défaut. Une annulation, une sélection inchangée ou un aperçu dont
l'affichage échoue ne doit pas autoriser une écriture. Les tests comparent
l'arborescence et les octets des fixtures, et pas seulement l'absence de backup.
Une définition MCP modifiée pendant une édition doit être revue, sans appliquer
à une nouvelle commande une autorisation donnée à l'ancienne.

Vérifier aussi les commandes directes `harness`/`harnesses`,
`skill`/`skills`, `mcp`/`mcps` et leur mode `--plain` (inventaire
sans écriture). Les cinq valeurs de `agentcfg completion` sont `bash`,
`zsh`, `fish`, `powershell` et `pwsh`; la sortie doit pouvoir être
chargée dans le shell cible sans lancer de MCP. Les commandes d'installation
sont dans [completion.md](completion.md).

`status --json` et `auth status <server> --json` restent du JSON même sur un
terminal interactif. Les erreurs d'écriture de la sortie doivent être remontées.
Les requêtes de statut respectent l'annulation et leur délai même après connexion
au broker, afin qu'un interlocuteur bloqué ne fige pas les menus.

## Installateurs et binaires

```sh
mise exec -- go test -count=1 -timeout=180s -v ./internal/installtest
```

Les tests d'installateurs utilisent des archives servies localement et un
manifeste `SHA256SUMS` de test. Ils contrôlent le rejet d'un hash invalide,
l'installation d'un exécutable `agentcfg`, son aide sans argument et la
réinstallation dans un répertoire temporaire. PowerShell est testé sur Windows ;
Bash est testé sur Unix et, si Git Bash est disponible, sur Windows.

`.github/workflows/check.yml` vérifie le formatage Go et exécute `go vet`,
`go test -shuffle=on`, un build CLI et un
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
2. `agentcfg init` demande les harnesses de destination, puis une seule confirmation avant l'import et la migration.
3. Après validation, les MCP JSON/JSONC et TOML Codex utilisent les proxies agentcfg ; les sources upstream n'existent que sous `~/.agentcfg/`.
4. Les skills natifs sont centralisés puis copiés physiquement dans les destinations choisies ; les anciennes sources, y compris celles des harnesses non sélectionnés, sont retirées avec sauvegarde.
5. `agentcfg init --yes` autorise le parcours complet ; `init --plain` ne modifie aucun fichier.
6. Un import incomplet ou ambigu bloque init. Une erreur pendant la migration tente de restaurer l'état natif et archive les sources importées. `restore` doit rétablir les configurations et skills d'origine.
7. Pour les modifications ultérieures, `agentcfg sync --plain` prévisualise les changements et `agentcfg sync --apply --yes` autorise leur application.

Lancer ensuite `agentcfg config`, puis vérifier que :

1. Configuration et Inventory présentent les quatre catégories avec compteurs numériques alignés, puis `Refresh` et `Exit`, via Huh Select. Le compteur Harnesses correspond aux harnesses activés, pas au nombre d'adaptateurs supportés ; Refresh recharge sans écrire.
2. Les cases cochées apparaissent seulement dans les listes de disponibilités multiples ; annuler une édition ne modifie rien.
3. Les entrées Diagnostics et Synchronize sont absentes du menu principal ; `agentcfg status`, `agentcfg validate` et `agentcfg sync` restent disponibles. La prévisualisation de synchronisation reste en lecture seule avant confirmation et application.
4. Les détails, avertissements et conflits sont lisibles sur le terminal cible.

Rejouer ces parcours avec des inventaires vides, un seul élément et plus de
vingt éléments ; inclure des MCP et skills nommés `back`, `exit` et `refresh`.
Réduire puis agrandir le terminal, utiliser la recherche, atteindre le dernier
élément et revenir plusieurs fois au menu principal. Vérifier l'absence de
lignes vides accumulées, `Exit` rouge, `NO_COLOR` et le mode accessible avec
plusieurs réponses fournies dans le même flux. Une fin d'entrée doit annuler,
jamais boucler ou valider implicitement.

Les tests automatisés utilisent des processus et des serveurs factices. Ils ne
prouvent pas la compatibilité de bout en bout de chaque version de Claude Code,
Codex, Cursor, OpenCode, Gemini ou Antigravity. Les opérations de configuration
doivent être vérifiées avec les applications réelles avant d'affirmer une telle
compatibilité.
