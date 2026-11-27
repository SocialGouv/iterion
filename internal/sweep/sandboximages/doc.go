// Package sandboximages holds the guards that hold the sandbox images and the
// runner image to the bots that run on them: the sec image's exact deepsec pin
// and its fail-closed sanity step, the browser image's Chromium contract with
// rgaa-audit, and the ultra11y version every image and the bot agree on.
//
// They read sandbox/*/Dockerfile, Dockerfile.runner-devbox and .dockerignore,
// which a dependency bump edits often. They live outside bots so that such a
// bump selects this small package instead of the whole bots package.
package sandboximages
