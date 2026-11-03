# Évolutions différées

## Synchronisation bidirectionnelle

Non implémentée. La détection de modifications manuelles actuelle doit rester
la base du mécanisme de conflits. Un futur import devra distinguer les entrées
personnelles des relais générés et reconstruire la configuration upstream, pas
importer `agentcfg connect` comme serveur réel.

Prévoir une fusion à trois versions : canonique actuelle, dernière projection
connue et configuration native actuelle. Les suppressions, renommages, filtres,
secrets, changements d’arguments et collisions entre harnesses devront être
présentés dans un plan explicite. Aucun « dernier écrivain gagne » silencieux.

## Autres améliorations possibles

Un mode `doctor` pourrait comparer les chemins natifs détectés avec les
projections attendues et signaler les entrées orphelines, permissions, binaires
manquants et erreurs de broker sans lancer de MCP. Le `sync` actuel fournit
déjà un **dry-run en lecture seule** ; une évolution utile serait une
prévisualisation structurée des différences, ligne par ligne, avec
classification des risques. Une restauration guidée depuis les sauvegardes
existantes demanderait une validation des empreintes avant tout remplacement,
sans effacer les modifications postérieures. Un export de diagnostics pourrait
regrouper les résultats de validation, versions, événements et statuts en
masquant les secrets, headers et données des serveurs.

Ces possibilités sont **proposées, pas implémentées** dans la version actuelle.

Tests de conformité approfondis du MCP 2026-07-28 (MRTR, tâches,
subscriptions, fournisseurs réels) ; stockage natif OAuth Keychain macOS et
Secret Service Linux en complément du chiffrement explicite actuel ; profils et
chemins personnalisés par harness ; import explicite des configurations natives
(distinct de `agentcfg migrate`, limité aux anciennes sources canoniques) ; mode watch
unidirectionnel ; métriques RAM/processus ;
politique de rétention des logs ; compatibilité skills avec liens internes
contrôlés ; provenance signée des binaires de release.

Les tests futurs avec les applications réelles doivent compléter, et non
remplacer, les tests de protocole et de fichiers temporaires existants.
