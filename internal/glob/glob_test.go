package glob

import "testing"

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		// The default exclusions.
		{"**/*.lock", "yarn.lock", true},
		{"**/*.lock", "web/yarn.lock", true},
		{"**/*.lock", "a/b/c/Cargo.lock", true},
		{"**/*.lock", "yarn.lock.bak", false},
		{"**/*.lock", "locked", false},
		{"**/migrations/**", "db/migrations/001_init.sql", true},
		{"**/migrations/**", "migrations/001_init.sql", true},
		{"**/migrations/**", "a/b/migrations/c/d/e.sql", true},
		{"**/migrations/**", "db/migration/001.sql", false},
		{"**/migrations/**", "db/my-migrations/001.sql", false},
		{"**/*secret*", "config/secret.yaml", true},
		{"**/*secret*", "my-secrets.json", true},
		{"**/*secret*", "a/b/client_secret_x.go", true},
		{"**/*secret*", "config/settings.yaml", false},
		{"**/*secret*", "secretdir/settings.yaml", false}, // * never crosses a directory

		// No slash: matches at any depth.
		{"*.lock", "a/b/yarn.lock", true},
		{"*.lock", "yarn.lock", true},
		{"go.sum", "svc/go.sum", true},

		// Anchored patterns.
		{"internal/*.go", "internal/app.go", true},
		{"internal/*.go", "internal/sub/app.go", false},
		{"internal/*.go", "other/internal/app.go", false},
		{"internal/**", "internal/a/b.go", true},
		{"internal/**", "internal", true},
		{"internal/**/x.go", "internal/x.go", true},
		{"internal/**/x.go", "internal/a/b/x.go", true},
		{"internal/**/x.go", "internal/a/b/y.go", false},
		{"**", "anything/at/all", true},
		{"a/**/**/b", "a/b", true},
		{"a/**/**/b", "a/x/y/b", true},

		// Syntax.
		{"file?.go", "file1.go", true},
		{"file?.go", "file12.go", false},
		{"[ab].go", "a.go", true},
		{"[ab].go", "c.go", false},
		{"[^ab].go", "c.go", true},

		// Case and path normalization.
		{"**/*SECRET*", "Config/My-Secret.YAML", true},
		{"**/*.lock", "./yarn.lock", true},
		{"**/*.lock", "/yarn.lock", true},
		{"/internal/*.go", "internal/app.go", true},

		// Never matches nothing.
		{"**/*.lock", "", false},
		{"*", "", false},
	}
	for _, tt := range tests {
		p, err := Compile(tt.pattern)
		if err != nil {
			t.Fatalf("Compile(%q): %v", tt.pattern, err)
		}
		if got := p.Match(tt.name); got != tt.want {
			t.Errorf("%q matches %q = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestCompileRejectsBadPatterns(t *testing.T) {
	for _, bad := range []string{"", "   ", "[abc", "a/[/b", "a//b", "**/[", `a\`} {
		if _, err := Compile(bad); err == nil {
			t.Errorf("Compile(%q) should fail", bad)
		}
	}
}

func TestSet(t *testing.T) {
	set, err := CompileAll([]string{"**/*.lock", "**/migrations/**"})
	if err != nil {
		t.Fatal(err)
	}
	if !set.Match("db/migrations/1.sql") || !set.Match("x.lock") || set.Match("main.go") {
		t.Error("set matching is wrong")
	}
	if (Set{}).Match("anything") {
		t.Error("an empty set must match nothing")
	}
	if _, err := CompileAll([]string{"**/*.lock", "[bad"}); err == nil {
		t.Error("CompileAll must fail on an invalid pattern")
	}
	if p, _ := Compile("**/*.lock"); p.String() != "**/*.lock" {
		t.Errorf("String() = %q", p.String())
	}
}

// Patterns come from the operator but names come from scanners; make sure a
// hostile name cannot make matching run away.
func TestMatchIsBoundedOnPathologicalInput(t *testing.T) {
	p, err := Compile("**/a/**/a/**/a/**/b")
	if err != nil {
		t.Fatal(err)
	}
	name := ""
	for i := 0; i < 25; i++ {
		name += "a/"
	}
	if p.Match(name + "c") {
		t.Error("unexpected match")
	}
}
