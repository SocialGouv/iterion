---
name: glossaire-produit
description: How to build and maintain the per-product glossary page — which terms earn an entry, how each entry is written and grounded in the sources, and how pages link to it. Overridden by the docs repository's own .product-docs/ glossary when it publishes one.
---

# Glossaire du produit

> Ce cadre est le **défaut du bundle**. Si le dépôt de documentation publie
> son propre glossaire ou ses propres règles de vocabulaire
> (`.product-docs/*.md`), c'est cela qui fait foi : le vocabulaire d'un
> produit appartient à l'équipe produit, pas au générateur.

Le glossaire est une page du produit (`<produit>/glossaire.md`). Il existe
pour une seule raison : **un même objet métier doit porter le même nom sur
toutes les pages, et ce nom doit être celui de l'interface**. Sans lui, chaque
page réinvente un synonyme et le lecteur croit lire deux choses différentes.

## Quels termes méritent une entrée

Un terme entre au glossaire s'il remplit **au moins deux** de ces conditions :

- il apparaît dans l'interface du produit (libellé, statut, intitulé de
  bouton, en-tête de colonne) ;
- il désigne un objet ou un état propre au produit, pas un mot courant ;
- il est utilisé sur plus d'une page ;
- son sens diffère du sens ordinaire du mot, ou d'un usage voisin dans un
  autre produit de la même organisation.

N'entrent pas : les termes techniques (ils n'ont pas leur place dans ces
pages), les mots du langage courant, les acronymes internes que l'utilisateur
final ne rencontre jamais.

## Comment s'écrit une entrée

```markdown
### Nom du terme

Définition en une à trois phrases, du point de vue de celui qui l'utilise.
Ce que c'est, à quoi ça sert, à quel moment on le rencontre.

Voir aussi : [terme lié](#terme-lie), [l'étape où il apparaît](parcours/etape.md)
```

- Le terme est écrit **exactement** comme l'interface l'écrit — même
  singulier/pluriel, mêmes majuscules, même accentuation.
- La définition ne contient pas le terme lui-même (« un dossier est un
  dossier qui… »).
- La définition ne décrit pas l'implémentation. « Statut que prend la demande
  une fois l'instruction terminée » ; jamais « valeur de l'énumération ».
- Les entrées sont classées par ordre alphabétique.
- Un synonyme réellement utilisé par les utilisateurs mérite sa propre entrée
  courte renvoyant à l'entrée principale.

## Ancrer une définition dans les sources

Comme toute affirmation de ces pages, une définition est **sourcée ou
`[à confirmer]`**. Le vocabulaire réel se lit surtout dans :

- les catalogues de traduction / fichiers i18n — la source la plus fiable,
  c'est littéralement ce que l'utilisateur voit ;
- les gabarits d'écran, d'e-mail et de notification ;
- les libellés des états et des transitions (machines à états, énumérations
  exposées à l'écran) ;
- les messages de validation et d'erreur affichés.

Quand deux sources se contredisent (un ancien libellé subsiste dans un
gabarit), retenez celui que l'utilisateur voit aujourd'hui et signalez la
contradiction dans le compte rendu de rédaction — pas dans la page.

Quand aucune source ne permet de trancher le sens d'un terme, écrivez la
définition avec `[à confirmer]` dans la phrase concernée. N'inventez jamais
une définition plausible : une définition fausse contamine toutes les pages
qui l'emploient.

## Lier depuis les pages

À sa **première occurrence dans une page**, un terme du glossaire est écrit en
italique et lié à son entrée :

```markdown
La demande passe alors au statut *[en instruction](../glossaire.md#en-instruction)*.
```

Les occurrences suivantes de la même page ne sont pas liées : un texte truffé
de liens ne se lit plus.

## Entretien

Le glossaire suit le produit comme les autres pages : quand une source montre
qu'un libellé a changé, l'entrée est corrigée **et** les pages qui l'emploient
sont mises à jour dans la même passe. Un glossaire à jour et des pages qui
utilisent l'ancien mot valent moins que pas de glossaire du tout.

Un terme qui a disparu de l'interface n'est pas supprimé sans vérification :
il peut encore être connu des utilisateurs. Conservez l'entrée en indiquant
par quoi il a été remplacé.
