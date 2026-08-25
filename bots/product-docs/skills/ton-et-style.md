---
name: ton-et-style
description: Default tone and style rules for published functional pages — person, tense, sentence shape, the sourced-or-[à confirmer] discipline, and how to touch human-validated prose. Overridden by the docs repository's own .product-docs/ style guide when it publishes one.
---

# Ton et style

> Ces règles sont le **défaut du bundle**. Si le dépôt de documentation publie
> sa propre charte (`.product-docs/*.md`), c'est elle qui fait foi.

## Le ton

Vous écrivez pour quelqu'un qui a une tâche à accomplir et peu de temps. Le
ton est **factuel, direct et neutre** : ni promotionnel, ni scolaire, ni
familier. On explique ce que le produit fait ; on ne le vend pas, on ne
s'excuse pas de ses limites, on ne se félicite pas de ses fonctionnalités.

## Les règles de forme

- **Vouvoiement**, quand on s'adresse au lecteur. Pas de « nous ».
- **Présent de l'indicatif.** « La demande passe au statut *en instruction* »,
  jamais « la demande passera » ni « la demande va passer ».
- **Voix active.** « Le gestionnaire valide le dossier », pas « le dossier est
  validé par le gestionnaire ».
- **Une idée par phrase.** Au-delà de deux lignes, coupez.
- **Les mots de l'interface, entre guillemets ou en gras**, tels qu'ils sont
  écrits à l'écran. N'inventez jamais un synonyme « plus clair » : le lecteur
  cherche le libellé qu'il a sous les yeux.
- **Pas de jargon technique** : ni API, ni base de données, ni déploiement, ni
  nom de fichier ou de service. Si le mot n'apparaît pas dans l'interface et
  n'est pas au glossaire, il n'a probablement pas sa place.
- **Pas d'acronyme non explicité** à sa première occurrence.
- **Les nombres et les délais s'écrivent tels que le produit les applique**
  (« 30 jours », pas « environ un mois »), et seulement s'ils sont sourcés.
- **Pas de méta-discours** : « cette page décrit… », « comme vu plus haut… »,
  « nous allons voir… ». Entrez dans le sujet.
- **Pas de références temporelles instables** : « récemment », « la nouvelle
  version », « prochainement ». Une page se lit deux ans plus tard.

## Sourcé, ou `[à confirmer]`

C'est la règle qui prime sur toutes les autres.

Chaque affirmation factuelle d'une page — une règle métier, un délai, un
statut, un droit d'accès, une condition de refus — est **ancrée dans une
source lue** (le code du produit) ou **dans une prose déjà validée par un
humain**. Quand ni l'un ni l'autre, deux options seulement :

1. ne pas l'écrire ;
2. l'écrire en marquant l'incertitude **dans la phrase** :

```markdown
Le dossier est archivé au bout de 30 jours [à confirmer].
```

Il n'y a pas de troisième option. Une phrase plausible mais non vérifiée est
indiscernable d'une phrase vérifiée pour le lecteur — c'est la faute la plus
grave possible ici, plus grave qu'une lacune.

Le marqueur `[à confirmer]` se place au plus près de l'élément incertain, pas
en tête de section. Une page entière marquée `[à confirmer]` est une page
qu'il ne fallait pas écrire.

## La prose déjà validée par un humain

Une grande partie de ces pages a été écrite ou corrigée par quelqu'un qui
connaît le produit mieux que ne l'enseigne une lecture du code. On n'y touche
que dans **deux cas** :

1. **le code la contredit** — on corrige, au plus près, et on l'explique dans
   le message de commit ;
2. **elle est incomplète** au regard d'un parcours que le code implémente
   clairement — on complète.

Ne reformulez jamais une phrase correcte parce que vous l'auriez tournée
autrement. Une passe qui réécrit de la prose qu'elle n'avait aucune raison de
toucher détruit du travail humain et ne produit rien.

Quand la prose validée et le code se contredisent et que **c'est le code qui
semble fautif**, ne réalignez pas la page sur le comportement du code :
signalez-le (`is_product_bug`) et laissez la page en l'état.

## Titres

- Un titre d'action est à l'infinitif : « Déposer une demande ».
- Un titre de page hub nomme le rôle ou le parcours : « Espace gestionnaire ».
- Pas de ponctuation finale, pas de numérotation manuelle.
- Un seul titre de niveau 1 par page, qui est le titre de la page.

## Ce qu'on ne met jamais dans une page

Ni commentaire HTML, ni encadré « Sources », ni section « Points à
clarifier », ni annexe « Correspondance technique ». Les notes de travail, les
références de fichiers et les questions ouvertes appartiennent au message de
commit, au registre des promesses ou au rapport de la campagne — jamais à la
page que lit l'utilisateur.
