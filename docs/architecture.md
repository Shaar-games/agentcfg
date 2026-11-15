# Architecture et garanties

## Frontends natifs, backend partagé

Par défaut, la synchronisation remplace le lancement de chaque MCP par un relais
stdio `agentcfg connect`. Le nom de serveur reste visible dans le harness. Un
relais est nécessaire par connexion stdio, mais le processus lourd du serveur et
ses descendants peuvent être mutualisés. L'activation explicite de la gateway
projette les amonts HTTP sous forme d'URL natives ; les MCP lancés par commande
restent sur ce chemin stdio.

Le broker utilise un endpoint dérivé du chemin canonique de la racine source et
de l’utilisateur. Windows : named pipe protégé par une ACL pour le SID courant.
Unix : socket dans un répertoire 0700, socket 0600. Ce broker n'écoute pas de port
HTTP. La gateway optionnelle est un service distinct limité à la boucle locale.
Si le chemin du cache dépasse la limite du socket Unix, le broker utilise un
répertoire privé dans le dossier temporaire, puis dans `/tmp` si ce chemin reste
trop long. Ces replis conservent la même identité de racine et d’utilisateur,
ainsi que les contrôles de propriétaire et de permissions.

Un verrou de démarrage permet à plusieurs relais concurrents de vérifier puis
lancer le broker. Un deuxième verrou est conservé toute la vie du broker. Le
processus lancé est le même exécutable, en sous-commande `daemon`, détaché de la
console. Les journaux du broker se trouvent dans le même répertoire privé que
l’endpoint ; les diagnostics ne sont jamais envoyés sur stdout des relais.

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
clients suivants. Le backend représente agentcfg, pas le premier harness :
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

## Gateway HTTP et deux autorisations distinctes

`gateway.enabled` est faux par défaut. Quand il est activé, `sync` génère une URL
stable pour chaque nom de serveur HTTP et harness autorisé par `gateway.harnesses`.
Une liste absente ou vide couvre tous les harnesses sélectionnés ; les clients
exclus gardent le parcours stdio et son login manuel. La gateway relit les
sources canoniques pour résoudre ces routes. Elle ne réécrit ni l'URL amont,
ni ses en-têtes, ni son identité de credentials. Chaque session HTTP cliente
garde sa session amont ; ce chemin n'utilise pas le multiplexage du broker.

`gateway start` lance le service explicitement et `gateway run` le garde au
premier plan pour un service de session configuré par l'utilisateur. Le contrôle
de statut et d'arrêt reste sur un endpoint IPC privé distinct du broker.
`gateway stop` n'arrête pas les processus stdio. Aucune URL projetée, opération
de sync ou installation du binaire ne lance automatiquement la gateway.

Le premier démarrage crée une clé de routage aléatoire dans le magasin chiffré.
Chaque URL contient une capacité HMAC liée au serveur et au harness ; connaître
seulement leur nom ne permet pas d'obtenir l'accès. L'URL complète reste un secret
local. La synchronisation vérifie les droits des destinations avant le plan et
avant l'écriture : permissions et traversée des parents Unix, DACL et héritage
des fichiers temporaires Windows. Un fichier exposé est refusé sans changement
automatique de ses droits. La règle couvre aussi les copies dans les sauvegardes
et journaux, ainsi que la position finale d'un layout déplacé, même lorsque le
gateway vient d'être désactivé. Les projets, manifestes portables et paramètres JSON
canoniques ne reçoivent jamais la clé de routage.

Le client traite agentcfg comme une ressource MCP HTTP protégée par OAuth. Il
découvre l'émetteur local, s'enregistre par DCR et effectue un échange code/PKCE.
Le jeton local donne accès à une ressource précise et reste distinct du jeton
utilisé par agentcfg auprès du fournisseur amont. Le consentement local reste
nécessaire pour les amonts publics ; une autorisation fournisseur manquante
ajoute son propre parcours et son callback. Aucun outil d'authentification
synthétique n'est annoncé dans le protocole MCP.

La gateway écoute par défaut sur `127.0.0.1:43877`. Seules les adresses IP
loopback littérales et les ports non nuls sont acceptés. Une paire certificat/clé
optionnelle active HTTPS pour les clients qui l'exigent. Les contrôles Host,
Origin, cookie et CSRF protègent le parcours navigateur. Les codes et jetons
sont liés au client et à l'identité de la ressource ; les états OAuth locaux et
amont sont enregistrés dans des espaces distincts du magasin chiffré.

Le chemin stdio reste disponible pour les clients sans OAuth HTTP compatible :
`auth login` explicite, puis reconnexion. L'ouverture automatique du navigateur
depuis `connect` reste un choix séparé, désactivé par défaut.

## Ce que les tests démontrent

Le test d’intégration lance deux vrais exécutables `agentcfg connect` avec des
identités de harness distinctes et un serveur MCP de test exécuté comme processus
enfant. Il vérifie un seul PID upstream, un seul démarrage enregistré, deux
clients dans le statut, des requêtes utilisant le même identifiant, puis la
survie du backend après fermeture d’un client. D’autres tests vérifient les PIDs
distincts en mode isolé et l’arrêt après inactivité.

`TestNativeOAuthStandardSDKEndToEnd` utilise un vrai client HTTP du SDK MCP,
la gateway et un fournisseur OAuth local. Il vérifie les deux parcours OAuth,
le callback amont et un appel MCP authentifié, dans des fixtures temporaires.
Les tests de projection vérifient les six formats natifs et leur migration
depuis et vers les proxies stdio, sans toucher aux sources canoniques.

Il ne s’agit pas d’un test de bout en bout de l’interface des applications
Cursor, Codex ou Claude Code. Leur configuration est vérifiée par les tests des
adaptateurs ; leurs validations de confiance, redémarrages ou activations
manuelles restent à effectuer dans ces applications.
