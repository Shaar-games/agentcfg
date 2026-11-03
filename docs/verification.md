# Vérifications — 2 et 3 novembre 2026

## Contrôles locaux historiques (2 novembre)

Hôte d’exécution : Windows, Go 1.26.10 via `mise.toml`.

| Vérification exécutée | Résultat |
|---|---|
| `go test -count=1 -timeout=120s ./...` | Réussite sur tous les packages testés |
| `go test -race -count=1 -timeout=180s ./...` | Réussite ; aucune course signalée |
| `go vet ./...` | Réussite |
| `mise run check` | Réussite |
| `mise run build` | Réussite, `bin/agentmux.exe` |
| `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ...` | Réussite, `dist/agentmux-linux-amd64` |
| `GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ...` | Réussite, `dist/agentmux-darwin-arm64` |
| CLI `version`, `validate`, `sync --dry-run`, `status` | Réussite |

## Preuve de mutualisation

`TestCLISharedProcessAcrossTwoHarnesses` lance deux vrais processus CLI, portant
les identités `cursor` et `codex`, puis les relie au même MCP de test. Lors d’un
passage détaillé, le broker avait le PID **28036** et le seul MCP enfant le PID
**40748**. Ces numéros sont des observations de test, pas des processus à joindre
ultérieurement.

Le test vérifie également une seule ligne dans le journal des démarrages du MCP,
deux clients dans `status`, l’utilisation simultanée du même identifiant JSON-RPC
sans collision et une réponse du second client après fermeture du premier, avec
le même PID upstream. Les essais en mode isolé donnent deux PIDs différents.
L’arrêt du broker après inactivité est testé dans un scénario distinct.

## Régressions couvertes

Les tests couvrent notamment la conservation des commentaires JSONC et du TOML
non géré, les conflits manuels/concurrents, les suppressions limitées aux éléments
gérés, les liens/copies de skills, la non-redécouverte des copies de plugins, les
filtres, les jonctions Windows, les collisions d’identifiants/progression,
l’annulation, les timeouts, la mort d’un upstream, les wrappers batch et la
fermeture de descendants. HTTP est testé avec des endpoints `httptest` pour
JSON/SSE, callbacks, sessions, erreurs et redirections.

Une échappée par jonction Windows a été reproduite puis corrigée avec une
résolution native des chemins ; des tests forcés `mklink /J` couvrent maintenant
le chargeur et les destinations de synchronisation. Une régression de parsing
SSE CRLF et une mutation du tampon de référence JSONC ont également été corrigées
avant les passages complets ci-dessus.

## CI et release publiées (`v0.2.1`, 3 novembre)

Les contrôles complémentaires de la version publiée `604bce8` ont réussi :

| Preuve | Résultat |
| --- | --- |
| [CI `main` — exécution 41545732674](https://github.com/Shaar-games/agentmux/actions/runs/41545732674) | Succès Windows, Ubuntu et macOS |
| [CI du tag `v0.2.1` — 41546063962](https://github.com/Shaar-games/agentmux/actions/runs/41546063962) | Succès |
| [Release et smoke tests — 41546063960](https://github.com/Shaar-games/agentmux/actions/runs/41546063960) | Succès de la vérification, des builds, de la publication et des installations réelles sous Windows, Ubuntu et macOS |
| [Release publique `v0.2.1`](https://github.com/Shaar-games/agentmux/releases/tag/v0.2.1) | Six archives (Windows, Linux, macOS, chacune AMD64 et ARM64), plus `SHA256SUMS` |

Les vérifications locales précédant cette publication ont aussi réussi pour
`go test ./...`, `go test -race ./...`, `go test -shuffle=on ./...`, `go vet ./...`,
`go mod verify` et `actionlint`. Six builds croisés ont été effectués avec
`CGO_ENABLED=0` ; il ne s'agit pas d'une exécution sur six machines.

Un parcours Huh réel a été piloté au clavier sur Windows avec un home temporaire :
sélection des harnesses, changement `shared`/`isolated` d'un MCP existant,
édition de la disponibilité d'un skill, inventaire de plugins, diagnostics,
aperçu, annulation, application et vérification d'idempotence du sync. Les
fixtures de configuration couvrent Claude Code, Codex, Cursor, OpenCode et
Antigravity (ainsi que Gemini CLI). Les tests de synchronisation préservent
les entrées natives non gérées et vérifient les fichiers générés. Les
installateurs téléchargent des binaires et contrôlent leurs SHA-256 :
`install.ps1` a également été essayé avec une véritable archive publique
sous Windows, et `install.sh` sur Ubuntu/macOS dans la CI de release.

Les résultats de ces workflows correspondent au commit **`604bce8`**, non à
d'éventuels correctifs postérieurs. Une nouvelle révision de code demande
ses propres vérifications locales et une nouvelle CI.

## Revue corrective du 3 novembre (contrôles locaux distincts)

La revue finale a reproduit trois défauts de `agentmux init` : sélection de
harnesses invalides (`codex,unknown`, `vscode`, entrée vide intermédiaire),
`--harnesses ""` non reconnu et suivi d'une jonction Windows placée sous le
home. Les correctifs valident la sélection avant toute création, acceptent
une liste explicitement vide et refusent les liens/jonctions aux emplacements
canoniques. Un contrôle préalable des répertoires `skills` et `plugins` évite
aussi la création partielle de sources avant le rejet d'une jonction. Les
tests vérifient la conservation des sources existantes après une erreur.

Les contrôles suivants ont été relancés **après ces modifications** sous
Windows 11 / Go 1.26.10 :

| Contrôle | Résultat |
| --- | --- |
| `go test ./... -count=1 -timeout=180s` | Réussite |
| `go test -race ./... -count=1 -timeout=180s` | Réussite ; aucune course signalée |
| `go test -shuffle=on ./... -count=2 -timeout=180s` | Réussite |
| `go vet ./...` et `go mod verify` | Réussite |
| `actionlint` sur `check.yml` et `release.yml` | Réussite |
| `go build -trimpath`, `CGO_ENABLED=0` | Réussite des six couples Linux/macOS/Windows × AMD64/ARM64 |

La fixture Chrome DevTools possède désormais un `cwd` explicite vers
`~/.agentmux/`. Son test confirme le chemin résolu, sans lancer Node ni le
serveur MCP. Ces résultats locaux ne valent ni validation des applications
réelles ni preuve de succès d'une CI sur le nouveau commit : ces contrôles
externes doivent être examinés séparément après publication du code.

## Portée et limites

Les scénarios utilisent des clients de test et des fichiers temporaires. Les
applications Cursor/Codex/Claude/OpenCode/Antigravity elles-mêmes, Chrome
DevTools et le service Context7 réel n'ont pas fait l'objet de tests applicatifs
de bout en bout. La CI a exécuté nativement les tests Go et les installateurs
sous Windows, Ubuntu et macOS ; elle n'a pas exécuté l'interface Huh avec un
clavier sur ces deux derniers systèmes. Les archives ARM64 ont été compilées,
mais non testées sur du matériel ARM64. Les fixtures Antigravity valident les
chemins/formats attendus, pas toutes les versions réelles de l'application.

L'usage de l'officiel `github.com/modelcontextprotocol/go-sdk` ne garantit pas
la prise en charge de toutes les extensions MCP. Le mode `shared` conserve
potentiellement un état applicatif commun ; `isolated` ne constitue pas une
sandbox OS. Huh gère les harnesses, la disponibilité des skills, le mode des
MCP existants et la synchronisation, mais pas encore la création/édition de
toutes les définitions MCP ni l'installation de plugins.

Le scénario d'installation initiale du 2 novembre utilisait une configuration
canonique vide : zéro serveur et zéro skill actifs, cinq harnesses sélectionnés.
Sa prévisualisation annonçait zéro changement et le statut confirmait qu'aucun
broker n'était actif pour cette racine. Les exemples sont inactifs tant qu'ils
ne sont pas copiés dans les emplacements canoniques. Aucun `sync --apply` n'a
été exécuté sur les configurations réelles de l'utilisateur ; les applications
de plans réalisées pendant les tests ciblaient uniquement des répertoires
temporaires.
