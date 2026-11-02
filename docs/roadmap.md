# Évolutions différées

## Synchronisation bidirectionnelle

Non implémentée. La détection de modifications manuelles actuelle doit rester
la base du mécanisme de conflits. Un futur import devra distinguer les entrées
personnelles des relais générés et reconstruire la configuration upstream, pas
importer `agentmux connect` comme serveur réel.

Prévoir une fusion à trois versions : canonique actuelle, dernière projection
connue et configuration native actuelle. Les suppressions, renommages, filtres,
secrets, changements d’arguments et collisions entre harnesses devront être
présentés dans un plan explicite. Aucun « dernier écrivain gagne » silencieux.

## Autres améliorations possibles

Support des protocoles MCP 2026 remaniés après tests de conformité dédiés ;
OAuth distant avec stockage séparé des tokens ; profils et chemins personnalisés
par harness ; mécanisme d’import explicite ; mode watch unidirectionnel ;
restauration guidée des sauvegardes ; métriques RAM/processus ; limites et
politique de rétention des logs ; compatibilité skills avec liens internes
contrôlés.

Les tests futurs avec les applications réelles doivent compléter, et non
remplacer, les tests de protocole et de fichiers temporaires existants.
