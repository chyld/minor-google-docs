// Package pageassets holds the stylesheet, script and fonts of a note saved as a
// web page (SPEC §23). The Inter font files and their @font-face rules are copied
// from web/node_modules/@fontsource-variable/inter (OFL, see fonts/OFL.txt).
package pageassets

import "embed"

//go:embed page.css page.js fonts
var FS embed.FS
