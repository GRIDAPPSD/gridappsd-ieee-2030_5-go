// Package graphpage holds the static files of the Graph page. They are
// embedded so the bridge serves the page with no build step. The chart
// library is the copy vendored in package socpage.
package graphpage

import "embed"

// Files are index.html, app.js and style.css.
//
//go:embed index.html app.js style.css
var Files embed.FS
