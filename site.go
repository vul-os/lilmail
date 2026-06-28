package main

import "embed"

// siteFS holds the standalone marketing site (landing + docs viewer) served for
// lilmail.vulos.org. It is mounted read-only at /site/* so it never shadows the
// webmail app, which lives at / behind sign-in. The docs viewer fetches the
// markdown copied into site/docs/ from the same route it is served on.
//
//go:embed all:site
var siteFS embed.FS
