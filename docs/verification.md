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

## Serveur MCP npm réel et Docker

Les essais réseau sont activés explicitement. Depuis Linux ou WSL avec Docker
disponible :

```sh
./scripts/test-docker.sh
```

Le script construit une image avec Go 1.26.10 et Node.js 24.22.1, exécute la suite
complète, `go vet`, le détecteur de courses sur tous les packages, puis les
tests npm. Il ne monte aucun home, configuration MCP, source ou socket Docker
hôte dans le conteneur. Le code entre dans l'image par un contexte limité aux
sources, exemples et fixtures de test. Les homes, caches utilisateur, caches Go
et répertoires temporaires des tests sont placés dans un répertoire temporaire
du conteneur. Le conteneur utilise un utilisateur non privilégié ;
le conteneur et son tag d'image sont supprimés à la fin. `--skip-npx` omet les
essais npm et `--skip-race` omet le détecteur de courses.

Les mêmes essais npm peuvent être lancés sans Docker :

```sh
AGENTCFG_TEST_NPX=1 go test ./internal/upstream ./cmd/agentcfg \
  -run '^Test(NPXFilesystem|CLINPXFilesystem)' -count=1 -timeout=300s
```

Ils téléchargent le serveur officiel
`@modelcontextprotocol/server-filesystem@2.2.0` dans un cache temporaire. Node.js
24 ou plus récent et `npx` doivent être dans le `PATH`. Le premier test passe par
le transport stdio directement. Le second ajoute le proxy CLI et le broker, avec
deux clients en modes partagé et isolé. Ils contrôlent la découverte des outils,
une lecture avec accents et espaces dans le chemin, le refus d'un fichier hors
du dossier autorisé et la fermeture des processus. Le parcours CLI vérifie aussi
qu'un client continue après la déconnexion de l'autre.

Les sockets Unix doivent être autorisées sous Linux/macOS pour le broker et
Docker. Une interdiction de sockets bloque ces vérifications ; elle ne doit pas
être transformée en réussite ou en désactivation permanente des tests. Docker
Linux ne valide pas les wrappers Windows : lancer les tests stdio et l'opt-in
npm sous Windows pour vérifier réellement `npx.cmd`, `PATHEXT` et les Job Objects.

Les régressions de tramage stdio utilisent les mêmes octets avec des lectures
groupées ou fragmentées. Elles refusent les objets accolés sans séparateur, les
batches, les objets répartis sur plusieurs lignes physiques, l'UTF-8 invalide et
une dernière ligne sans délimiteur. Elles préservent les messages valides
précédents et acceptent les lectures octet par octet, les caractères UTF-8
fragmentés, LF et CRLF. La limite de 32 MiB est testée exactement, y compris pour
deux messages maximaux consécutifs, puis avec un octet de dépassement. Ces tests
et le transport ne dépendent d'aucune commande particulière ; `npx` sert à
l'intégration avec un serveur réel.

Les lecteurs stdio et IPC sont aussi vérifiés avec des grandes lignes découpées
en fragments fixes de 1 KiB, alternées avec une petite ligne. Ces cas contrôlent
l'intégrité du contenu et la reprise correcte à chaque délimiteur. La recherche
du séparateur conserve sa progression au lieu de reparcourir tout le préfixe
à chaque lecture. Les mesures comparatives peuvent être reproduites sans lancer
la suite de tests :

```sh
go test ./internal/upstream ./internal/wire -run '^$' \
  -bench 'Benchmark(StdioFramingFixedFragments|ReaderFixedFragments)$' \
  -benchtime=1x -count=1
```

Les tests OAuth locaux utilisent le SDK MCP officiel et un émetteur de test :
PKCE, scopes des challenges, validation d'émetteur, callbacks DCR, stockage
chiffré, connexion créée avant le login, logout puis nouveau login sur le même
transport, avec protocoles legacy/moderne et réponses JSON/SSE. Ils vérifient
aussi la priorité des en-têtes SDK et le repli legacy après une découverte
refusée, interrompue ou incompatible. Aucun compte OAuth personnel n'est utilisé.

Les parcours stdio passent aussi par le vrai broker et son IPC natif. Le flux
stdin n'est jamais traité comme une saisie de terminal ; stdout reste réservé
aux messages MCP amont. Les contrôles privés du navigateur restent sur IPC.
Les fixtures vérifient le login manuel puis la reconnexion, la réutilisation des
jetons, l'ouverture automatique désactivée par défaut et son activation
explicite. Le catalogue reste celui du serveur, sans outil d'authentification
synthétique. Un faux ouvreur contrôle méthode, chemin, state, port occupé,
annulation et diagnostics sans compte réel. Les tests conservent la priorité des
credentials explicites et l'absence de rejeu d'un outil après une erreur réseau
indéterminée.

`TestNativeOAuthStandardSDKEndToEnd` utilise un client HTTP inchangé du SDK MCP,
la vraie gateway et un fournisseur OAuth local. Il suit le challenge de la
ressource locale, l'inscription DCR, le consentement, les deux échanges PKCE et
le callback amont avant de découvrir les vrais outils et de faire un appel MCP.
Le client n'annonce aucune capacité d'elicitation. Le test compare les identités
et les jetons des deux parcours pour vérifier qu'un secret du fournisseur ne
devient pas un token client. Les fixtures utilisent des homes temporaires, un
magasin chiffré et un navigateur HTTP simulé ; elles ne lancent aucun navigateur
réel et ne consultent aucun compte personnel.

`TestNativeOAuthBrowserNavigation` est une régression distincte avec un vrai
Chrome/Chromium. Elle exige les chemins absolus du navigateur et de Node via
`-agentcfg-browser-executable` et `-agentcfg-browser-node` ; sans ces options,
elle est ignorée. Elle lance un profil temporaire, soumet le vrai formulaire,
suit le callback du fournisseur puis celui du client et échange le code PKCE.
Les cas de premier login et d'autorisation fournisseur déjà sauvegardée vérifient
l'en-tête Origin, l'absence de nouveau consentement fournisseur et l'absence de
violation CSP. La CI Windows fournit explicitement les deux exécutables.

```sh
go test ./internal/gateway -run '^TestNativeOAuthBrowserNavigation$' -count=1 -timeout=120s \
  -args -agentcfg-browser-executable=/absolute/path/to/chrome \
  -agentcfg-browser-node=/absolute/path/to/node
```

Les régressions de l'autorité locale couvrent les codes à usage unique, PKCE,
les redirects et ressources, Host/Origin/CSRF, le refus de consentement, la
rotation et le rejeu des refresh tokens, les échanges concurrents, l'arrêt avec
annulation et la persistance chiffrée après redémarrage. Les capacités des routes
sont liées au nom du serveur et au harness par HMAC ; les tests de configuration
refusent une clé manquante et empêchent sa sérialisation dans les paramètres.
Les contrôles du proxy concernent les sessions HTTP distinctes, les réponses
JSON/SSE et callbacks, ainsi que les challenges d'autorisation amont.
Le cas de scope supplémentaire vérifie que l'ancienne session MCP legacy finit
par HTTP 404 ; une reconnexion explicite réutilise les nouveaux credentials sans
troisième consentement ni rejeu incertain de l'outil.

Ces tests établissent le parcours OAuth natif avec le SDK utilisé par le dépôt.
Ils ne valident ni une interface précise de Cursor/OpenCode, ni la compatibilité
OAuth de toutes les versions de clients. HTTP loopback et HTTPS restent des
choix dont l'acceptation dépend du client.

Les régressions du broker vérifient qu'une écriture d'annulation bloquée n'arrête
ni l'expiration des autres opérations, ni la déconnexion, ni les réponses aux
ping locaux. Les annulations sont envoyées dans une file bornée, avec un motif
de taille limitée ; une saturation de cette file ferme le backend pour permettre
une reconnexion. Une saturation temporaire des emplacements HTTP de contrôle
abandonne seulement l'annulation consultative refusée. Un test avec le vrai
transport HTTP bloque quatre notifications, provoque deux refus `ErrBusy` et
vérifie que les deux clients, un appel indépendant et un nouvel appel restent
fonctionnels. Une erreur d'écriture fatale ferme toujours le backend.
Un serveur qui ne lit plus son stdin peut toujours bloquer une nouvelle écriture
vers ce serveur.

## Régressions de configuration et de synchronisation

Les projections gateway couvrent les six adaptateurs, l'activation puis la
désactivation, les filtres, un mélange Cursor HTTP/OpenCode stdio, les fichiers JSON/JSONC et TOML, les conflits manuels
et la restauration de la baseline d'init. Elles vérifient l'absence de données
amont dans l'URL locale, la stabilité du sync suivant et le maintien des sources
canoniques. L'import reconnaît seulement les routes gérées prouvées ; les MCP
localhost manuels restent disponibles et une clé absente ou changée bloque le
réimport d'une route déjà gérée. La suppression préalable du serveur canonique
ne transforme pas une ancienne route possédée en amont importable.

Les fixtures de projection Windows créent une DACL privée dans leurs homes
temporaires avant les fichiers. Les tests de permissions contrôlent aussi les
fichiers exposés et l'héritage du temporaire utilisé par l'écriture atomique.
Sur Unix, un fichier en mode `0644` sous un home en mode `0700` peut rester privé ; une modification
des parents après le plan doit être détectée avant application. Lorsque le
TMPDIR du runner est lui-même sous un ancêtre privé, le cas d'exposition réelle
le signale explicitement et les tests de chaînes de permissions vérifient la
décision sans changer les droits du runner.
Les régressions de récupération refusent une destination publique pour une
sauvegarde, un journal ou un rollback contenant une URL protégée, y compris
après désactivation du gateway ou retrait du dernier MCP HTTP. Les variantes
JSON/JSONC/TOML échappées, BOM et `Before` en base64 sont couvertes. Les copies
contrôlent les octets et le mode effectivement écrits ; le scénario de publication
d'un lien privé vers un home `0755` vérifie qu'une copie `0644` ne perd pas la
protection de son ancien ancêtre.

Les régressions de liens utilisent huit emplacements natifs de skills et leurs
56 paires orientées : liens absolus et relatifs sur Unix/Windows, plus jonctions
sur Windows, soit 168 parcours init → sync sans changement → restore sur Windows.
Des tests complémentaires couvrent les chaînes, les cibles hors du home, les
assets et SKILL.md liés, les fichiers MCP partagés, le TOML Codex, les parents
OpenCode/JSONC et le home lui-même lié. Les liens cassés et cycles sont refusés
sans modifier les sources. Les tests Windows nécessitant des symlinks signalent
un skip si le système ne permet pas leur création ; les jonctions sont testées
séparément sans ce privilège.

Les tests Windows ouvrent réellement le fichier ou le dossier de staging sans
partage de suppression pour reproduire `Access is denied` et les violations de
partage. Ils vérifient la reprise après libération, le verrou persistant borné,
l'annulation, le refus d'une destination étrangère, le remplacement MCP et la
restauration des skills legacy liés ou contenant une jonction et un asset en
lecture seule. Les journaux de staging des anciennes versions restent couverts.
La restauration revérifie aussi le contenu complet avant chaque tentative de
déplacement et de rollback. Quatre cas Windows modifient un descendant pendant
un verrou natif, y compris dans la sauvegarde originale ou pendant un rollback :
le conflit est signalé et les octets modifiés restent dans leur chemin.

Les plugins combinent des liens sur le répertoire canonique, le catalogue,
le package, les manifestes, les composants MCP, les commandes et les skills.
Les tests d'édition vérifient que les liens sources restent présents et qu'une
cible changée depuis l'aperçu est refusée. Les tests de récupération simulent
une copie interrompue, un échec de synchronisation, des fichiers de staging
étrangers et un échec tardif de restauration suivi d'une nouvelle tentative.

Les doublons MCP couvrent les six harnesses, les quatre fichiers OpenCode,
les définitions identiques et différentes, les noms de destination déjà pris,
les indicateurs et listes de désactivation ainsi que les overrides sans commande.
Chaque parcours vérifie les proxies natifs, la disponibilité canonique, l'absence
de changement au sync suivant et la restauration exacte des fichiers.
Les transitions suivantes couvrent aussi les listes de désactivation sans entrée
MCP, un fichier secondaire apparu après l'aperçu, la stabilité des noms quel que
soit l'ordre de découverte, la restauration après des synchronisations ultérieures
et le retour arrière du journal. Les fichiers étrangers et répertoires remplacés
pendant un échec restent intacts.


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
Les subprocess PowerShell ont aussi un profil, un répertoire courant et un
cache d'analyse des modules privés. Une régression vérifie les chemins vus par
PowerShell et les dossiers Windows connus, pour éviter un cache relatif dans
le dépôt même quand `LocalApplicationData` serait vide.

`.github/workflows/check.yml` vérifie le formatage Go et exécute `go vet`,
`go test -shuffle=on`, un build CLI et un
smoke test de l'aide sur Windows, Ubuntu et macOS. Le détecteur de courses est
également exécuté sur Linux. La même matrice configure Node.js 24.22.1 et active
explicitement les deux tests npm réels : transport stdio, puis CLI/broker en
modes partagé et isolé. Cette étape doit s'exécuter sur un runner Windows pour
valider le vrai `npx.cmd` ; sa présence dans le workflow ne prouve pas son succès.
`.github/workflows/release.yml` vérifie le code,
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

Les tests par défaut utilisent des processus et des serveurs factices. Ils ne
prouvent pas, même avec l'essai npm réel optionnel, la compatibilité de bout en
bout de chaque version de Claude Code,
Codex, Cursor, OpenCode, Gemini ou Antigravity. Les opérations de configuration
doivent être vérifiées avec les applications réelles avant d'affirmer une telle
compatibilité.
