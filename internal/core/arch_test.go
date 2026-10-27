package core

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestCoreHasNoNetworkOrDiskDependencies keeps the architecture honest: the use
// case and everything it depends on inside this module must be testable with
// fakes alone, so none of them may import anything that touches the network, the
// disk, a database or another process.
func TestCoreHasNoNetworkOrDiskDependencies(t *testing.T) {
	banned := map[string]bool{
		"net": true, "net/http": true, "net/url": false, "os": true, "os/exec": true, "io/ioutil": true,
		"database/sql": true, "syscall": true, "plugin": true,
	}
	// The core and the packages it may import in this module.
	dirs := []string{".", "model", filepath.Join("..", "ports"), filepath.Join("..", "glob")}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no sources in %s: %v", dir, err)
		}
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				if banned[path] {
					t.Errorf("%s imports %q", name, path)
				}
				// Nothing in the core may reach an adapter or a wrapper around git.
				for _, forbidden := range []string{"/adapters/", "/gitcli", "/safehttp", "/listener", "/config"} {
					if strings.Contains(path, "self-healing/internal") && strings.Contains(path, forbidden) {
						t.Errorf("%s imports %q", name, path)
					}
				}
			}
		}
	}
}
