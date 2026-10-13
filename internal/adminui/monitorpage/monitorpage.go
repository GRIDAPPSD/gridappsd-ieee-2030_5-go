// Package monitorpage holds the static files of the Bus monitor page. They
// are embedded so the bridge serves the page with no build step.
package monitorpage

import "embed"

// Files are index.html, app.js and style.css.
//
//go:embed index.html app.js style.css
var Files embed.FS
