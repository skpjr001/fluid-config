// Package ui holds the console's static files.
package ui

import "embed"

// FS holds index.html, app.js, app.css and the favicon.
//
//go:embed *.html *.js *.css *.svg
var FS embed.FS
