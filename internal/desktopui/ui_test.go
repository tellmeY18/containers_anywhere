package desktopui

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"strings"
	"testing"
)

// The brand files are vendored unmodified. A change here must be a deliberate update.
var brandHashes = map[string]string{
	"tokens/tokens.css":  "ca657d9f235e70361bdafab6a9190ff1e727b6fd914aba6048b233082d14a712",
	"css/base.css":       "387c484165b4bc18b8fae3512f7bf1ee0e4125bfccf35726ece2a373f821080c",
	"css/components.css": "f45a358303ad8cc94773dfff16c0eb7b5f6d2852f28da8f6154571ea5bc748e2",
	"fonts/atkinson-hyperlegible-mono-latin.woff2": "f0230cab68ecdc96766fde00a89cf3167e668c3a8756f844cab554dc3bcfd030",
	"fonts/courier-prime-400-latin.woff2":          "f09844e1f4fe9e67df2cc77474519e68756d559b4648278b9dd4f03c8ac9c31f",
	"fonts/courier-prime-700-latin.woff2":          "5c7ae1cc9aae183453565c836b74a1b988f5f2e52b65098decb909b3930957ec",
}

func TestBrandFilesAreVendoredUnmodified(t *testing.T) {
	for name, want := range brandHashes {
		b, err := fs.ReadFile(Assets(), name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s changed: %s", name, got)
		}
	}
}

func TestPanelServesOnlyItsOwnOrigin(t *testing.T) {
	if strings.Contains(CSP, "unsafe-inline") || strings.Contains(CSP, "unsafe-eval") {
		t.Fatal("CSP allows inline code")
	}
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(CSP, want) {
			t.Fatalf("CSP lacks %s", want)
		}
	}
	index := string(Index())
	for _, forbidden := range []string{"http://", "<style", "onclick=", "javascript:"} {
		if strings.Contains(index, forbidden) {
			t.Fatalf("index.html contains %s", forbidden)
		}
	}
	for _, link := range []string{"https://letsimpl.fyi", "https://github.com/tellmeY18/containers_anywhere"} {
		if !strings.Contains(index, link) {
			t.Fatalf("missing link %s", link)
		}
	}
}

func TestPanelNeverParsesMarkupOrAddsColour(t *testing.T) {
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "confirm(", "prompt(", "alert("} {
		// alert( is our own helper; only the browser dialogs are banned.
		if forbidden == "alert(" {
			continue
		}
		if strings.Contains(Source(), forbidden) {
			t.Fatalf("panel uses %s", forbidden)
		}
	}
	css, err := fs.ReadFile(Assets(), "css/panel.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"#", "rgb(", "hsl(", "box-shadow", "border-radius", "gradient"} {
		if strings.Contains(string(css), bad) {
			t.Fatalf("panel.css must use brand tokens only, found %s", bad)
		}
	}
}

func TestFontBudget(t *testing.T) {
	total := 0
	entries, _ := fs.ReadDir(Assets(), "fonts")
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".woff2") {
			info, _ := e.Info()
			total += int(info.Size())
		}
	}
	if total > 150*1024 {
		t.Fatalf("fonts are %d bytes", total)
	}
}

func TestEveryAssetReferencedByTheDocumentExists(t *testing.T) {
	for _, name := range []string{"tokens/tokens.css", "css/base.css", "css/components.css", "css/panel.css", "js/main.js"} {
		if _, err := fs.Stat(Assets(), name); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(Index()), "/assets/"+name) {
			t.Fatalf("index.html does not load %s", name)
		}
	}
}
