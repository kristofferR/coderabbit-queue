package serve

import (
	"embed"
	"io/fs"
)

// The SPA is embedded from web/dist. The directory is committed with a
// placeholder so a source checkout still builds before anyone runs the
// frontend toolchain — Assets() reports "not built" instead of failing.
//
//go:embed all:dist
var dist embed.FS

// Assets returns the built SPA, or nil when only the placeholder is present.
func Assets() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
