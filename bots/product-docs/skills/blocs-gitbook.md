---
name: blocs-gitbook
description: Default GitBook block vocabulary for published functional pages — which blocks are allowed, what each one means to the reader, and which markup is forbidden. Overridden by the docs repository's own .product-docs/ framing when it publishes one.
---

# Blocs autorisés dans une page publiée

> Ce vocabulaire est le **défaut du bundle**. Si le dépôt de documentation
> publie sa propre liste de blocs (`.product-docs/*.md`), c'est celle-là qui
> fait foi.

Les pages sont du Markdown, publiées par GitBook. Le jeu de blocs ci-dessous
est volontairement petit : chaque bloc a un sens pour le lecteur, et un bloc
utilisé pour faire joli lui apprend à ignorer les suivants.

## Markdown standard — la base

Titres (`##`, `###`), paragraphes, listes à puces et numérotées, gras pour un
libellé d'interface, liens relatifs, tableaux. C'est ce qui doit porter 90 %
du contenu.

- **Gras** : un libellé exact de l'interface (« cliquez sur **Envoyer** »).
- *Italique* : un terme du glossaire à sa première occurrence dans la page.
- Tableau : un ensemble fini de cas comparables (statuts, rôles, délais).
  Jamais pour mettre en page du texte.

## Les encadrés (`hint`)

Quatre types, quatre sens distincts. Un encadré signale ce qu'un lecteur
pressé ne doit pas rater — il ne répète pas le paragraphe qui précède.

```
{% hint style="info" %}
Une précision utile mais non bloquante.
{% endhint %}
```

- `info` — une précision, une exception bénigne, un délai à connaître.
- `success` — la confirmation qu'une étape est bien terminée.
- `warning` — une action irréversible, une condition qui bloque, une erreur
  fréquente. C'est celui qui a le plus de valeur : réservez-le.
- `danger` — une conséquence grave (perte de données, refus définitif).
  Presque jamais nécessaire.

**Un encadré ne contient jamais de référence de source.** Un encadré
« Sources : … » est une note de travail : la sonde éditoriale le refuse et
fait échouer la passe.

Deux encadrés qui se suivent sont un signe qu'il fallait un paragraphe.

## Les étapes numérotées (`stepper`)

Pour un parcours strictement ordonné à l'intérieur d'une page.

```
{% stepper %}
{% step %}
### Déposer la demande
Ce que fait l'utilisateur à cette étape.
{% endstep %}
{% endstepper %}
```

À réserver aux vraies séquences imposées. Une liste numérotée ordinaire suffit
dans la plupart des cas, et se lit mieux.

## Les onglets (`tabs`)

Pour une même étape vécue différemment selon le profil ou le canal (par
exemple « depuis un ordinateur » / « depuis un mobile »).

```
{% tabs %}
{% tab title="Depuis un ordinateur" %}
…
{% endtab %}
{% endtabs %}
```

Si les onglets décrivent deux parcours différents, ce sont deux pages.

## Les images

Une capture d'écran n'est utile que si elle montre quelque chose que le texte
ne peut pas dire. Elle vieillit vite : elle doit rester compréhensible même
légèrement décalée par rapport à l'interface. Texte alternatif toujours
renseigné. Une capture qui contient des données réelles (nom, numéro de
dossier, adresse) ne doit jamais être publiée.

## Ce qui est interdit

- **Tout commentaire HTML** (`<!-- … -->`). Aucune exception : c'est une note
  de travail qui a survécu à la relecture.
- **Un encadré ou une section « Sources »**, sous quelque forme que ce soit.
- **Une section « Points à clarifier »** : une question ouverte s'écrit
  `[à confirmer]` dans la phrase concernée.
- **Une annexe « Correspondance technique »** : le contenu technique n'a pas
  sa place dans ces pages.
- Du HTML brut pour la mise en page, des styles inline, des balises `<br>`.
- Un bloc de code, sauf s'il montre quelque chose que l'utilisateur final
  saisit ou reçoit littéralement (un identifiant de dossier, un message
  d'erreur affiché à l'écran).

Les quatre premiers points sont vérifiés par une sonde déterministe après
chaque passe : une seule occurrence fait échouer la passe et vous revient.
