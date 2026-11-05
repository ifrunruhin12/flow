package migrations

import "embed"

// Files contains the reviewed SQL migration pairs embedded into the backend.
//
//go:embed *.sql
var Files embed.FS
