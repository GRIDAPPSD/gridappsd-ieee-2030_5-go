// Package healthpage holds the static files of the Bridge health page. They
// are embedded so the bridge serves the page with no build step.
package healthpage

import "embed"

// Files are index.html, app.js and style.css.
//
//go:embed index.html app.js style.css
var Files embed.FS
