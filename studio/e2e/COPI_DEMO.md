# Démo Copi : créer, lancer et superviser un workflow

Cette démo Playwright raconte un seul scénario continu dans le vrai Studio :

1. une demande en français est envoyée à Copi ;
2. Copi propose de créer le workflow `demo-healthcheck` ;
3. après confirmation, Copi propose de lancer son premier run ;
4. Copi arme une veille durable sur ce run ;
5. le run rencontre une erreur intentionnelle et devient reprenable ;
6. Copi reçoit l'événement, diagnostique l'échec et propose la reprise ;
7. après confirmation, le run reprend et se termine avec succès ;
8. la veille réveille une dernière fois Copi, qui confirme le résultat.

## Lancer la présentation

Installer Chromium une seule fois si nécessaire :

```sh
task test:e2e:ui:install
```

Puis lancer la démo en fenêtre avec un rythme lisible :

```sh
task demo:copi
```

La pause entre les grandes étapes vaut 1,2 seconde par défaut. Elle peut être
ajustée sans changer le test :

```sh
ITERION_E2E_DEMO_DELAY_MS=2000 task demo:copi
```

## Ce que la démo prouve

Le dialogue de Copi est un fixture déterministe : il ne dépend ni d'un compte
LLM ni du réseau, afin que la présentation soit reproductible. En revanche, les
cartes d'action et leurs validations, la création du bot, le lancement du run,
le retry moteur, l'état `failed_resumable`, la veille serveur, la reprise et la
fin du run utilisent tous les vrais chemins produit.

Le faux endpoint OpenAI n'écoute que sur loopback. Il reste en mode erreur
jusqu'à la confirmation de reprise, puis renvoie une sortie structurée valide.
Le test vérifie deux appels en erreur avant la reprise et exactement un appel
réussi après, ainsi que l'identité du run à chaque étape.

La suite UI normale n'active pas ce fixture : le scénario est opt-in via
`ITERION_COPI_DEMO=1`, que `task demo:copi` positionne automatiquement.
