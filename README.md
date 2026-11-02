# agentmux

Un gestionnaire local de **processus MCP partagés** et de **skills**, écrit en Go.
Chaque harness conserve une entrée native par serveur MCP. Seule la commande
change : un petit relais `agentmux connect` rejoint un broker local qui démarre
le serveur réel une seule fois pour la même racine de configuration.

```text
Cursor  ── chrome-devtools: agentmux connect ──┐
Codex   ── chrome-devtools: agentmux connect ──┼── IPC local ── broker ── chrome-devtools × 1
Claude  ── chrome-devtools: agentmux connect ──┘                 │
                                                            └── context7 × 1
Cursor  ── context7: agentmux connect ─────────────────────────┘
```

**Aucun serveur HTTP local, aucun port TCP en écoute.** Le transport local est
un named pipe Windows ou un socket Unix. Un serveur MCP distant peut néanmoins
être joint en HTTP sortant. Le broker démarre à la première connexion et s’arrête
après inactivité. Il n’est pas installé comme service système.

## Démarrer

La configuration canonique est **globale** : `~/.agentmux/` (sur Windows,
`$HOME\.agentmux\`). Par défaut, `agentmux init`, `list`, `connect`, `status`
et `sync` utilisent le dossier personnel, indépendamment du projet courant.
`sync` cible les configurations utilisateur par défaut (`--scope user`).
Le dossier `.agentmux/` d'un projet n'est pas découvert automatiquement.

### Installation en une commande

Prérequis : **Git** et **mise** ([installation de mise](https://mise.jdx.dev/getting-started.html)).
Les scripts clonent le dépôt dans un emplacement stable, installent Go via
`mise.toml` et compilent le binaire. Ils refusent d'écraser une installation
existante et ne modifient pas les configurations des harnesses.

**Linux / macOS (POSIX shell)** :

```sh
curl -fsSL https://raw.githubusercontent.com/Shaar-games/agentmux/main/install.sh | sh
```

**Windows PowerShell** :

```powershell
irm https://raw.githubusercontent.com/Shaar-games/agentmux/main/install.ps1 | iex
```

Sur Linux, macOS et Windows, les sources et fichiers de build sont conservés
dans `~/.local/share/agentmux`, tandis que l'exécutable est accessible dans
`~/.local/bin/agentmux` (ou `agentmux.exe` sous Windows). Sous Unix, il s'agit
d'un lien symbolique vers le binaire compilé ; sous Windows, d'une copie.
Ajouter `~/.local/bin` au `PATH` s'il n'y figure pas. Pour choisir le dossier
des sources, définir `AGENTMUX_INSTALL_DIR` avant l'installation ; le dossier
des exécutables reste `~/.local/bin`. Les scripts refusent d'écraser un binaire
existant. **Sécurité :** examiner `install.sh` ou `install.ps1` avant
d'exécuter un script distant ; ces commandes suivent la branche `main`.

### Développement depuis les sources

Après compilation, `agentmux init` initialise `~/.agentmux/`, puis
`agentmux sync` prévisualise la synchronisation globale et
`agentmux sync --apply` l'applique. `--root` permet de cibler explicitement
un autre dossier personnel pour les tests ou installations particulières.

```powershell
mise trust
mise install
mise run check
mise run build

# Dans le projet qui contiendra tes configurations canoniques :
.\bin\agentmux.exe init --root C:\chemin\mon-projet
.\bin\agentmux.exe list --root C:\chemin\mon-projet
.\bin\agentmux.exe validate --root C:\chemin\mon-projet

# Toujours une prévisualisation par défaut ; aucun fichier modifié :
.\bin\agentmux.exe sync --root C:\chemin\mon-projet

# Écrit les configurations natives et installe les skills :
.\bin\agentmux.exe sync --root C:\chemin\mon-projet --apply

# Lecture seule, sans démarrer de broker :
.\bin\agentmux.exe status --root C:\chemin\mon-projet
.\bin\agentmux.exe stop --root C:\chemin\mon-projet
```

Sous Linux/macOS, le binaire construit est `./bin/agentmux`. Les versions d’outils
sont fixées par `mise.toml`, les dépendances par `go.mod` et `go.sum`.
Utiliser un binaire installé à une adresse stable : les fichiers générés
enregistrent son chemin absolu. Ne pas appliquer une synchronisation depuis un
exécutable temporaire créé par `go run`, sauf avec `--binary /chemin/stable/agentmux`.

## Organisation des sources

```text
mon-projet/
  .agentmux/
    agentmux.json                    # Harnesses, runtime, filtres des plugins/skills
    mcp.json                         # Serveurs MCP canoniques
    skills/
      code-review/
        SKILL.md
        references/...
    plugins/
      mon-plugin/
        plugin.json                  # Manifest Agent Plugins portable
        mcp.json                     # Serveurs MCP du plugin
        skills/
          mon-skill/SKILL.md
    data/plugins/mon-plugin/          # PLUGIN_DATA, créé à l’exécution seulement
  .agentmux/                         # État de synchronisation et sauvegardes
```

Les exemples dans `examples/` ne sont pas chargés automatiquement dans tes
harnesses. Charger une configuration ne lance jamais de commande, n’installe
aucun paquet et ne crée pas les répertoires de données des plugins.

### `.agentmux/mcp.json`

```json
{
  "mcpServers": {
    "chrome-devtools": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "chrome-devtools-mcp@latest"],
      "sharing": "shared",
      "harnesses": ["cursor", "codex", "claude-code"]
    },
    "context7": {
      "type": "streamable-http",
      "url": "https://mcp.context7.com/mcp"
    }
  }
}
```

Cet exemple nécessite Node/npm pour Chrome DevTools. `@latest` est pratique pour
un exemple ; fixer une version vérifiée avant un usage reproductible. La commande
n’est exécutée que lorsqu’un client se connecte. Le navigateur et son état sont
communs aux clients en mode partagé : utiliser `"sharing": "isolated"` lorsque
deux agents doivent piloter des navigateurs indépendants.

`harnesses` et `sharing` sont des extensions **agentmux**, pas des ajouts au schéma
portable Agent Plugins. Dans le `mcp.json` d’un plugin, conserver le standard et
placer ces réglages dans les overrides ci-dessous. Les identifiants importés d’un
plugin sont `nom-du-plugin.nom-du-serveur` ou `nom-du-plugin.nom-du-skill`.

### `.agentmux/agentmux.json`

```json
{
  "harnesses": ["claude-code", "codex", "cursor", "opencode", "gemini"],
  "runtime": {
    "idleTimeout": "5m",
    "requestTimeout": "5m"
  },
  "serverOverrides": {
    "mon-plugin.browser": {
      "harnesses": ["cursor", "codex"],
      "sharing": "isolated"
    }
  }
}
```

Les skills utilisent `metadata.harness: codex,claude-code,cursor` dans leur
`SKILL.md` pour limiter les harnesses. Sans ce champ, ils ciblent tous les
harnesses configurés. `skillOverrides` n'est plus accepté.

Les overrides MCP doivent désigner des éléments réellement découverts ; sinon
`validate` signale un avertissement. Une liste globale `harnesses: []` ne génère
rien. Un filtre absent ou vide sur un élément signifie « tous les harnesses
sélectionnés ». Les filtres choisissent les destinations ; ce ne sont pas des
autorisations de sécurité entre logiciels du même utilisateur.

### Résultat dans un harness

Exemple pour Cursor : deux entrées distinctes, sans catalogue MCP agrégé.

```json
{
  "mcpServers": {
    "chrome-devtools": {
      "type": "stdio",
      "command": "C:\\outils\\agentmux.exe",
      "args": ["connect", "--root", "C:\\projets\\demo", "--server", "chrome-devtools", "--harness", "cursor"]
    },
    "context7": {
      "type": "stdio",
      "command": "C:\\outils\\agentmux.exe",
      "args": ["connect", "--root", "C:\\projets\\demo", "--server", "context7", "--harness", "cursor"]
    }
  }
}
```

Codex reçoit des tables TOML ; OpenCode reçoit des commandes sous forme de
tableaux. Les schémas natifs ne sont donc pas remplacés par un format commun.

## Harnesses et skills

| Harness | MCP, portée projet | Destination skills |
|---|---|---|
| Claude Code | `.mcp.json` | `.claude/skills/` |
| Codex | `.codex/config.toml` | `.agentmux/skills/`, natif |
| Cursor | `.cursor/mcp.json` | `.cursor/skills/` |
| OpenCode | `opencode.json` ou `opencode.jsonc` existant | `.opencode/skills/` |
| Gemini CLI | `.gemini/settings.json` | `.gemini/skills/` |
| VS Code, sélection explicite | `.vscode/mcp.json` | `.github/skills/` |

`--scope user` cible les fichiers utilisateur plutôt que les fichiers projet ;
VS Code reste limité à la portée projet dans cette version. Les emplacements
utilisateur personnalisés par des variables de harness ou des profils ne sont
pas détectés automatiquement.

Les skills ne sont pas des serveurs à démarrer. `--skill-mode auto` crée des
liens symboliques vers les répertoires canoniques et copie leur contenu complet
si les liens ne sont pas disponibles. `copy` force la copie ; `symlink` exige un
lien et échoue plutôt que de copier. Les mises à jour d’un lien sont immédiates ;
une copie nécessite une nouvelle commande `sync --apply`.

**Limite des filtres de skills :** les harnesses qui découvrent nativement
`.agentmux/skills` peuvent lire des sources même sans lien généré pour eux. agentmux
ne supprime pas les sources pour masquer un skill. Deux plugins dont les skills
ont le même nom natif provoquent un conflit explicite, pas un écrasement.

## Préservation des configurations

La synchronisation est unidirectionnelle : sources `.agentmux` vers fichiers
natifs. Elle préserve les entrées non gérées, les commentaires JSONC et les octets
TOML hors de son bloc. Les modifications et suppressions portent uniquement sur
des éléments suivis par le manifeste `.agentmux/sync-<scope>.json` et restés
inchangés depuis leur génération. Un conflit ou une modification concurrente
interrompt l’application. Des sauvegardes sont conservées sous
`.agentmux/backups/` dans la portée cible.

Une entrée existante portant le même nom n’est pas adoptée automatiquement.
L’import des configurations existantes et la synchronisation bidirectionnelle
sont **reportés**, avec résolution des conflits à concevoir. Il n’y a pas de
surveillance automatique des fichiers dans cette première version.

## Limites assumées de la première version

- Le partage est **par utilisateur et par racine source**, pas entre projets
  indépendants. Tous les clients d’un backend partagent son identité distante,
  ses variables et son état applicatif. Un changement de configuration crée une
  nouvelle génération pour les nouvelles connexions ; les connexions déjà
  ouvertes ne sont pas migrées. Redémarrer le broker après changement de son
  environnement hérité ou de ses réglages runtime.
- Le mode partagé couvre les requêtes MCP classiques, mais ne prétend pas isoler
  l’état métier. Roots, sampling, elicitation, subscriptions, niveau de logging
  par client et cycle des tâches nécessitent `sharing: isolated`. Les identifiants
  de requêtes et de progression sont remappés ; les appels mutateurs ne sont
  jamais rejoués automatiquement après un échec.
- Le transport HTTP implémente le cycle MCP **2025** initialise/session,
  réponses JSON ou SSE. Pas d’ancien transport SSE autonome, pas d’OAuth
  interactif, pas de reprise SSE par `Last-Event-ID`, pas de revendication de
  compatibilité avec les transports/procédures MCP 2026 remaniés.
- Sous Windows, les wrappers `.cmd`/`.bat` acceptent un sous-ensemble prudent
  d’arguments ; les cas impossibles à transmettre sans interprétation sont
  refusés. Un serveur `.exe` natif garde les arguments complets. Le broker doit
  pouvoir sortir du Job Object du premier harness pour lui survivre.
- Les MCP/plugins sont du code de confiance exécuté avec les droits de
  l’utilisateur. Les contrôles de chemins ne constituent pas un sandbox. Ne pas
  publier les clés/API tokens, les données des plugins, les sauvegardes ni les
  logs. Les arbres de skills contenant des liens internes sont refusés par le
  synchroniseur dans cette version.

## Développement et documentation

```text
cmd/agentmux/       CLI et tests d’intégration avec de vrais processus
internal/config/   Sources canoniques, Agent Plugins, validation des skills
internal/ipc/      Named pipes privés / sockets Unix
internal/wire/     Trames JSON-RPC bornées et écritures concurrentes
internal/broker/   Démarrage, partage, routage, statut et cycle de vie
internal/upstream/ Processus stdio, HTTP sortant, fermeture des descendants
internal/syncer/   Adaptateurs, plan, sauvegardes, skills, conflits
```

`mise run check` lance tests et analyse statique. `go test -race ./...` ajoute la
détection des courses lorsque le compilateur C adapté est disponible. Les tests
utilisent des répertoires temporaires, des subprocessus de test et des serveurs
HTTP locaux ; ils ne modifient pas tes configurations de harness existantes et
ne démarrent ni Chrome ni un serveur Context7 réel.

Voir [configuration](docs/configuration.md), [synchronisation](docs/synchronization.md),
[architecture](docs/architecture.md), [transports](docs/transports.md) et
[évolutions](docs/roadmap.md). Les contrôles réellement exécutés et leurs limites
sont consignés dans [le rapport de vérification](docs/verification.md).

Formats de référence : [Agent Plugins](https://agent-plugins.org/plugin-authors/mcp-servers),
[règles de chargement](https://agent-plugins.org/client-implementers/loading-and-discovery),
[Agent Skills](https://agentskills.io/specification) et
[MCP 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25).
Les [révisions MCP ultérieures](https://modelcontextprotocol.io/specification)
ne sont pas couvertes par une affirmation de conformité de ce projet.
