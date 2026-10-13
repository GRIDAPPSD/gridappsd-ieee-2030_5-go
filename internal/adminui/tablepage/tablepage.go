// Package tablepage holds the static files shared by the Bridge table
// pages (Connections, DER programs and Control flow). They are embedded so
// the bridge serves the pages with no build step.
package tablepage

import "embed"

// Files are index.html, table.js and table.css.
//
//go:embed index.html table.js table.css
var Files embed.FS
