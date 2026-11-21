// Package desktopui embeds the launcher panel: the letsimpl.fyi brand system
// (vendored unmodified, see BRAND.md) plus the panel's own markup and scripts.
package desktopui

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

//go:embed all:assets
var files embed.FS

// Assets is the tree served under /assets/.
func Assets() fs.FS {
	sub, err := fs.Sub(files, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}

// Index is the panel document.
func Index() []byte {
	b, err := files.ReadFile("assets/index.html")
	if err != nil {
		panic(err)
	}
	return b
}

// CSP is the policy for every panel response. All code, styles, fonts and icons
// are served from the panel's own origin.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Source returns the markup and scripts concatenated, for tests that assert on
// what the panel can and cannot do. It excludes the vendored brand files.
func Source() string {
	var names []string
	fs.WalkDir(files, "assets", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".js")) {
			names = append(names, p)
		}
		return nil
	})
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		data, _ := files.ReadFile(n)
		b.WriteString("\n/* " + n + " */\n")
		b.Write(data)
	}
	return b.String()
}
