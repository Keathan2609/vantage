// Package migrations embeds the SQL schema migrations into the binary.
//
// The migrations ship inside the executable so that a deployed control plane
// can never disagree with its own schema: there is no separate migration
// artifact to forget to copy, and no directory to mount.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
