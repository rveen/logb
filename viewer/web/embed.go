// Package web carries the built browser UI.
//
// dist/ is committed on purpose. The Go toolchain will not run a bundler, so
// embedding a build output means the build output has to be in the repository
// for `go install .../cmd/logbview@latest` to work without a Node toolchain.
// Rebuild it with `npm install && npm run build` in this directory after
// changing anything under src/.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

// The all: prefix matters. Without it embed silently skips files whose names
// begin with a dot or an underscore, which is exactly the kind of omission
// that shows up as a blank page rather than a build error.
//
//go:embed all:dist
var files embed.FS

// Handler serves the built UI.
func Handler() http.Handler {
	sub, err := fs.Sub(files, "dist")
	if err != nil {
		// Only reachable if the embedded tree is missing, which is a build
		// error rather than a runtime condition.
		panic("viewer/web: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}
