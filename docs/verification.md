# Vérification locale — 2 novembre 2026

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

## Portée des résultats

Les scénarios utilisent des clients de test et des fichiers temporaires. Les
applications Cursor/Codex/Claude elles-mêmes, Chrome DevTools et le service
Context7 réel n’ont pas été exécutés. Linux et macOS ont été compilés, pas exécutés
sur leurs systèmes respectifs. Le workflow GitHub Actions est présent, mais n’a
pas été publié ni lancé sur GitHub.

Le projet est livré avec une configuration canonique vide : zéro serveur et
zéro skill actifs, cinq harnesses sélectionnés. La dernière prévisualisation
annonce zéro changement et le statut confirme qu’aucun broker n’est actif pour
cette racine. Les exemples sont inactifs tant qu’ils ne sont pas copiés dans les
emplacements canoniques. Aucun `sync --apply` n’a été exécuté sur les configurations
réelles de l’utilisateur ; les applications de plans réalisées pendant les tests
ciblaient uniquement leurs répertoires temporaires.
