// Package socpage holds the static files of the SoC and readings page. They
// are embedded so the bridge serves the page with no build step.
package socpage

import "embed"

// Files are index.html, app.js, style.css and the vendored uPlot script and
// style (see NOTICE).
//
//go:embed index.html app.js style.css uplot.min.js uplot.min.css
var Files embed.FS
