# Architecture et garanties

## Frontends natifs, backend partagé

La synchronisation remplace seulement le lancement de chaque MCP par un relais
stdio `agentmux connect`. Le nom de serveur reste visible dans le harness. Un
relai est nécessaire par connexion stdio, mais le processus lourd du serveur et
ses descendants sont mutualisés.

Le broker utilise un endpoint dérivé du chemin canonique de la racine source et
de l’utilisateur. Windows : named pipe protégé par une ACL pour le SID courant.
Unix : socket dans un répertoire 0700, socket 0600. Aucun port réseau d’écoute.

Un verrou de démarrage permet à plusieurs relais concurrents de vérifier puis
lancer le broker. Un deuxième verrou est conservé toute la vie du broker. Le
processus lancé est le même exécutable, en sous-commande `daemon`, détaché de la
console. Les journaux du broker se trouvent dans le répertoire cache agentmux de
l’utilisateur ; les diagnostics ne sont jamais envoyés sur stdout des relais.

## Identité et cycle de vie

Le cache des backends utilise le nom du MCP et l’empreinte de sa configuration
résolue. Les connexions venant d’une autre racine ne partagent pas le backend.
Les variables héritées sont celles du broker à son lancement ; les overrides
explicites du serveur sont appliqués par le transport. Changer ces variables
héritées exige un arrêt puis un redémarrage du broker.

Les sources sont relues à chaque nouvelle connexion. Un changement de commande,
arguments, environnement ou URL peut donc produire une nouvelle génération.
L’ancienne continue de servir ses clients existants avant de s’éteindre après
inactivité. Les réglages d’inactivité du broker sont lus à son démarrage.

Un backend isolé est fermé dès la déconnexion de son client. Un backend partagé
reste vivant pendant `idleTimeout` sans clients. Le broker entier ferme ses
backends et quitte après sa propre inactivité. `stop` cible uniquement la racine
globale de l’utilisateur courant.

Les processus stdio sont placés dans un Job Object kill-on-close sous Windows,
avant la reprise de leur premier thread. Unix utilise un groupe de processus.
Les transports ferment leurs pipes et attendent la fin des enfants. Le broker
Windows tente le lancement breakaway pour ne pas mourir avec le Job Object du
premier harness ; si le parent refuse le détachement, un démarrage de secours
est tenté avec diagnostic explicite. Ce secours ne garantit pas la même
indépendance vis-à-vis de la durée de vie du parent.

## Multiplexage partagé

Les clients MCP 2025 utilisent l'ancien handshake stdio, mais le backend partagé
ne reçoit qu’une seule initialisation. Le résultat est conservé pour les
clients suivants. Le backend représente agentmux, pas le premier harness :
les capacités client ambiguës roots/sampling/elicitation ne sont pas annoncées.

Les clients MCP 2026-07-28 passent directement des requêtes autonomes avec
`params._meta` et peuvent utiliser `server/discover`. Leurs identifiants sont
remappés par client sans `initialize` global. Un backend partagé ne mélange pas
les deux générations de protocole sur la même connexion stdio. Cela évite les
collisions de transport, **pas** les conflits d'état applicatif d'un MCP.

Pour chaque requête, le broker conserve le propriétaire et l’identifiant
original, génère un identifiant upstream unique, puis restaure l’identifiant au
retour. Les `progressToken` sont transformés de la même façon ; les annulations
ne ciblent que la requête du client émetteur. Les changements de listes de tools,
prompts ou resources sont diffusés aux clients initialisés. Cela ne fournit pas
une isolation des données métier d’un serveur partagé.

Les opérations globales de session — subscriptions, logging par client, tâches —
sont refusées dans ce mode. Les callbacks serveur non annoncés reçoivent une
erreur explicite. Le mode isolé transmet le protocole sans remappage vers un
backend dédié et laisse le client gérer ses capacités.

Une trame est limitée à 32 MiB, un backend à 1 024 requêtes en attente, et le
broker à 128 connexions simultanées. Un client dont la file de sortie est pleine
est déconnecté afin de ne pas bloquer les autres. Un timeout produit une erreur
et une tentative d’annulation ; il ne prouve pas qu’un effet distant a été
annulé. Aucun appel n’est rejoué automatiquement.

## Ce que les tests démontrent

Le test d’intégration lance deux vrais exécutables `agentmux connect` avec des
identités de harness distinctes et un serveur MCP de test exécuté comme processus
enfant. Il vérifie un seul PID upstream, un seul démarrage enregistré, deux
clients dans le statut, des requêtes utilisant le même identifiant, puis la
survie du backend après fermeture d’un client. D’autres tests vérifient les PIDs
distincts en mode isolé et l’arrêt après inactivité.

Il ne s’agit pas d’un test de bout en bout de l’interface des applications
Cursor, Codex ou Claude Code. Leur configuration est vérifiée par les tests des
adaptateurs ; leurs validations de confiance, redémarrages ou activations
manuelles restent à effectuer dans ces applications.
