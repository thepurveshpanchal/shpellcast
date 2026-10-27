package server

import (
	"embed"
	"fmt"
	"path"
)

// Assets are compiled into the binary so that deploying shpellcast is
// copying one file. There is no asset directory to forget, no template
// path to configure and nothing to break if the working directory moves.
//
//go:embed web
var webFS embed.FS

func asset(name string) ([]byte, bool) {
	// path.Clean plus the Base call blocks traversal attempts such as
	// /static/../../etc/passwd before they reach the embedded FS.
	clean := path.Base(path.Clean("/" + name))
	b, err := webFS.ReadFile("web/" + clean)
	if err != nil {
		return nil, false
	}
	return b, true
}

func mustAsset(name string) []byte {
	b, ok := asset(name)
	if !ok {
		panic(fmt.Sprintf("shpellcast: embedded asset %q missing", name))
	}
	return b
}
