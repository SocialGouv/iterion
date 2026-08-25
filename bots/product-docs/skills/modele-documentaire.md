---
name: modele-documentaire
description: Default documentary model for functional product pages — structure by user roles and journeys, one hub page per role/journey and one sub-page per step. Overridden by the docs repository's own .product-docs/ model when it publishes one.
---

# Modèle documentaire par défaut

> Ce modèle est le **défaut du bundle**. Si le dépôt de documentation publie
> son propre modèle (`.product-docs/*.md`), c'est celui-là qui fait foi :
> lisez-le d'abord et appliquez-le intégralement.

La documentation fonctionnelle d'un produit se structure par **rôles** et par
**parcours**, jamais par modules techniques. Le lecteur arrive avec une
question de la forme « je suis X, je veux faire Y » — l'arborescence doit y
répondre en deux clics.

## L'arborescence

```
<produit>/
  README.md                     ← page d'accueil du produit
  <role-ou-parcours>/
    README.md                   ← page HUB du rôle / parcours
    <etape-1>.md                ← une sous-page par étape
    <etape-2>.md
  glossaire.md                  ← vocabulaire du produit
```

Trois niveaux suffisent presque toujours. Un quatrième niveau est le signe
qu'une étape est en réalité un parcours : promouvez-la.

## La page d'accueil du produit (`README.md`)

Trois choses, dans cet ordre :

1. **À quoi sert le produit**, en trois à cinq phrases, du point de vue de
   celui qui l'utilise. Pas d'historique, pas d'architecture, pas de liste de
   fonctionnalités.
2. **Qui l'utilise** — un tableau des rôles, avec pour chacun un lien vers son
   hub. C'est la table de routage du lecteur.
3. **Par où commencer** — le parcours le plus courant, en un lien.

## La page hub (`<role-ou-parcours>/README.md`)

Le hub répond à « qu'est-ce que je peux faire ici, et dans quel ordre ». Il
raconte le parcours ; il ne le détaille pas.

1. **En une phrase** : ce que ce rôle (ou ce parcours) permet de faire.
2. **Le parcours de bout en bout** : la suite des étapes, chacune en une
   phrase, chacune liée à sa sous-page. Une liste numérotée quand l'ordre est
   imposé, une liste à puces quand il ne l'est pas.
3. **Ce qu'il faut avoir avant de commencer** : accès, pièces, informations —
   uniquement si le produit l'exige réellement.
4. **Les cas particuliers** connus, en une ligne chacun, liés à leur page.

Un hub qui dépasse une page-écran est un hub qui documente au lieu de router.

## La sous-page d'étape (`<etape>.md`)

Une étape = une page. Une page = une chose qu'on fait, du début à la fin.

1. **Titre** : l'action, à l'infinitif, avec les mots de l'interface
   (« Déposer une demande », pas « Dépôt de dossier »).
2. **Quand faire cette étape**, et par qui — une ou deux phrases.
3. **Comment faire** : les actions dans l'ordre, telles qu'on les vit dans
   l'interface. Les libellés cités entre guillemets sont les libellés réels.
4. **Ce qui est demandé** : champs obligatoires, pièces, formats, limites —
   uniquement ce que le produit contrôle vraiment.
5. **Ce qui se passe ensuite** : le nouvel état, qui est notifié, quel est le
   délai s'il en existe un, quelle est l'étape suivante (avec son lien).
6. **Ce qui peut bloquer** : les refus possibles et la manière d'y remédier.
   C'est la section que les lecteurs cherchent le plus, et celle qu'on oublie
   le plus souvent.

## Règles de rédaction structurelle

- **Une page = une intention.** Si vous écrivez « par ailleurs » ou « à
  noter », vérifiez que la suite ne mérite pas sa propre page.
- **Chaque page est atteignable.** Toute page est liée depuis son hub, et tout
  hub depuis l'accueil. Une page orpheline est une page invisible — la sonde
  `orphan_page` la signale.
- **Les liens sont relatifs** entre pages du produit, jamais absolus.
- **Un titre décrit ce que le lecteur va faire**, pas la structure du produit.
- **Pas de sommaire écrit à la main** dans une page : la navigation vit dans
  les hubs.
- **Une information n'est écrite qu'à un seul endroit.** Ailleurs, on lie.
  Deux copies d'une règle métier divergent au premier changement.

## Ce qui n'apparaît jamais dans une page publiée

Ni commentaire HTML, ni encadré « Sources », ni section « Points à
clarifier », ni annexe « Correspondance technique ». Ces éléments sont des
notes de travail : une porte d'entrée fonctionnelle ne montre pas les
coulisses de sa fabrication. Une question ouverte s'écrit `[à confirmer]`
dans la phrase concernée ; le reste va dans le message de commit ou dans le
rapport de la campagne.
