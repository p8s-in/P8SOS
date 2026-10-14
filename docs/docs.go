// Package docs holds the P8S OS Administrator Guide (docs/guide/*.md and its
// pictures). It is embedded in p8sd (served at /docs/) and built into a
// standalone website by cmd/p8s-docs. The other files in docs/ are developer notes.
package docs

import "embed"

// Guide is the guide's source: guide/NN-slug.md (one per chapter) and guide/*.svg.
//
//go:embed guide/*.md guide/*.svg
var Guide embed.FS
