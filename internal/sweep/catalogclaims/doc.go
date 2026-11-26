// Package catalogclaims holds the guards that hold the bot catalogue to
// texts outside bots/: the doctrine pages its whole-tree staging must match,
// the pages that may not overclaim the empty-gate pre-check, the docs that
// describe deepsec's runtime, the studio and server sources that own Copi's
// protocol literals, and the studio's mirror of the category vocabulary.
//
// Each reads a named file outside bots/ (docs, studio, pkg/server), so a
// change to that file reaches it. They live outside bots so that such a
// change selects this small package instead of the whole bots package.
package catalogclaims
