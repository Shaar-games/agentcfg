# Synchronisation et propriété des fichiers

## Autorité

Les fichiers canoniques `~/.agentcfg/mcp.json`, `~/.agentcfg/agentcfg.json`, les plugins
et les skills sous `~/.agentcfg/` sont les seules sources d’autorité. `sync` prépare un plan en
lecture seule. `sync --apply` applique ce plan. Il n’existe ni import implicite,
ni mode force, ni fusion bidirectionnelle dans cette version.

agentcfg projette ces sources dans les **fichiers de configuration natifs** des
harnesses sélectionnés. Pour une migration d'anciennes sources ou de manifestes
d'appartenance, consulter [le guide de migration](../MIGRATION_FOR_AGENTS.md).

Pour passer de l'ancien nom au nouveau, `agentcfg migrate` prévisualise en
lecture seule une copie de `~/.agentmux/` vers `~/.agentcfg/` ; seul
`agentcfg migrate --apply` publie la nouvelle configuration canonique. Le
répertoire d'origine et son exécutable restent intacts. Cette migration ne
réécrit aucune configuration native : il faut examiner séparément le résultat
de `agentcfg sync` avant d'utiliser `agentcfg sync --apply`.

Un serveur MCP est généré individuellement dans chaque harness sélectionné.
Les filtres de l’entrée canonique et les overrides des plugins sont évalués
avant la génération. Un filtre vide signifie tous les harnesses globalement
sélectionnés ; une sélection globale vide retire seulement les éléments déjà
gérés et inchangés lors de la prochaine application.

## Éditions conservatrices

Le manifeste de propriété `~/.agentcfg/sync-user.json`
conserve les chemins et empreintes des entrées générées. Il ne contient pas la
configuration upstream ni ses secrets. Supprimer ce manifeste ne réinitialise
pas magiquement les fichiers : les entrées restantes redeviennent inconnues et
leurs collisions sont refusées.

Pour JSON/JSONC, les patches ciblent chaque entrée sous `mcpServers`, `mcp` ou
`servers`. Les commentaires sont conservés, même si l’indentation est normalisée
lors d’une modification. Pour TOML, un bloc délimité par commentaires contient
les tables générées ; les octets en dehors de ce bloc sont conservés. Modifier
ce bloc à la main produit un conflit, y compris pour ses commentaires.

Le plan refuse une entrée de même nom qu’il ne possède pas, un contenu géré
modifié ou supprimé manuellement, un chemin cible traversant un lien, ou une
destination de skill ambiguë. Les fichiers existants et leurs empreintes sont
revérifiés avant l’écriture. Les suppressions ne concernent que les éléments
gérés restés inchangés.

L’application utilise un verrou par portée, des écritures par fichier temporaire
puis renommage, et des sauvegardes sous `~/.agentcfg/backups/<date>/`. Une erreur
d’écriture déclenche une tentative de restauration des changements déjà faits.
Ce n’est pas une transaction de système de fichiers globale : une coupure
machine pendant l’application peut nécessiter une restauration depuis ces
sauvegardes. Une restauration ne remplace pas un fichier modifié concurremment.

## Skills

Le dossier complet d’un skill est lié ou copié, pas seulement `SKILL.md`.
`auto` tente un lien symbolique et copie si le système le refuse. `symlink`
refuse de continuer lorsqu’un lien n’est pas disponible. `copy` convient lorsque
le harness ou le système ne suit pas les liens. Aucune réécriture des noms du
frontmatter n’est effectuée ; les collisions entre noms natifs doivent être
résolues dans les sources.

La source `~/.agentcfg/skills/` est distincte des destinations natives, notamment
`~/.agents/skills/` pour Codex. Le contenu de chaque fichier du skill source,
dont `SKILL.md`, est conservé octet pour octet en mode copie. Aucun fichier de
marquage n'est ajouté aux nouvelles copies. Les anciens marqueurs
`.agentcfg-origin.json` restent reconnus à la lecture pour compatibilité.

Les liens internes dans un arbre de skill ne sont pas synchronisés dans cette
version, même lorsqu’ils restent dans le plugin. Cela évite les copies ambiguës
ou les références qui s’échappent de la destination. Un dossier source complet
peut en revanche être résolu avant création du lien de destination.

Les filtres ne peuvent pas empêcher un harness de scanner de lui-même les
destinations natives, par exemple `~/.agents/skills/`. Ils ne constituent donc pas une ACL. Les sources ne
sont jamais effacées pour contourner le scanner natif d’un logiciel.

## Portée utilisateur

Les instructions canoniques `~/.agentcfg/AGENTS.md` sont synchronisées en portée
utilisateur vers `~/.codex/AGENTS.md`, `~/.claude/CLAUDE.md` et
`~/.config/opencode/AGENTS.md`, sous réserve du filtre YAML `metadata.harness`.
La source est préservée ; le frontmatter propre à agentcfg est retiré des copies
générées. Les fichiers natifs non gérés ne sont jamais écrasés, et une modification
concurrente empêche l'application. Les autres harnesses reçoivent un avertissement
tant qu'aucune destination d'instructions utilisateur n'est vérifiée.

La CLI publique synchronise exclusivement les fichiers du home courant :
Claude utilise `.claude.json`, Codex
`.codex/config.toml`, Cursor `.cursor/mcp.json`, Gemini `.gemini/settings.json`
et OpenCode `.config/opencode/opencode.json` (ou `.jsonc` existant). Les tests
injectent systématiquement un faux home temporaire.

Google Antigravity utilise `.gemini/config/mcp_config.json` et les skills sous
`.gemini/config/skills/`. Les profils personnalisés ne sont pas détectés.
L'adaptateur VS Code existant est limité au projet et n'est donc pas actif dans
la CLI globale. `--root` et `--scope` ne sont pas exposés : le broker et la
configuration canonique sont identifiés par le home courant, sans modifier
les projets ouverts.

## Références de formats

- Claude Code : https://code.claude.com/docs/en/mcp et https://code.claude.com/docs/en/skills
- Codex : https://developers.openai.com/codex/mcp et https://developers.openai.com/codex/skills
- Cursor : https://cursor.com/docs/context/mcp et https://cursor.com/docs/context/skills
- OpenCode : https://opencode.ai/docs/mcp-servers/ et https://opencode.ai/docs/skills/
- Gemini CLI : https://geminicli.com/docs/tools/mcp-server/ et https://geminicli.com/docs/cli/skills/
- VS Code : https://code.visualstudio.com/docs/copilot/customization/mcp-servers et https://code.visualstudio.com/docs/copilot/customization/agent-skills

Les adaptateurs sont testés sur des fichiers de ces formats. Ces références ne
valident pas automatiquement chaque version installée des applications.
