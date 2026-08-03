package main

import (
	"embed"
	"io/fs"
)

//go:generate ../../scripts/sync-renderer2.sh
//go:embed embedded/dist
var embeddedRenderer embed.FS

func embeddedRendererFS() fs.FS {
	dist, err := fs.Sub(embeddedRenderer, "embedded/dist")
	if err != nil {
		panic(err)
	}
	return dist
}
