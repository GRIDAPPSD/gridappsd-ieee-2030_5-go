// Package senderpage holds the static files of the Bus publishing and Bus
// sender pages. They are embedded so the bridge serves the pages with no
// build step.
package senderpage

import "embed"

// Files are the two pages (switch.html and sender.html), their scripts, the
// section renderer both share, and the style sheet.
//
//go:embed switch.html switch.js sender.html sender.js sections.js style.css
var Files embed.FS
