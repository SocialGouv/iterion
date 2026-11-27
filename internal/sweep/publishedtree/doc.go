// Package publishedtree holds the guards over what this repository publishes
// as a whole: no tracked file under bots/, examples/, docs/, skills/,
// scripts/, charts/, ci/ or the root markdown names a third party's
// infrastructure, and the root .gitignore keeps claude_code's runtime junk
// out of every bot's `git add -A`.
//
// They read the tracked tree through git ls-files and git check-ignore, so a
// change to almost any published file may reach them. They live outside bots
// so that a docs, CI or chart change selects this small package instead of
// the whole bots package.
package publishedtree
