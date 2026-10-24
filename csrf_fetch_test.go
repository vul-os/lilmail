package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The CSRF middleware answers 403 to a POST/PUT/PATCH/DELETE without the
// double-submit X-CSRF-Token header. htmx requests get it from app.js, but a raw
// fetch() has to send it itself: "Add account" and "Save draft" did not and were
// always rejected. Every mutating fetch() in the templates and scripts must
// mention the header (the web-push routes are exempt from the middleware).
func TestMutatingFetchSendsCSRFToken(t *testing.T) {
	mutating := regexp.MustCompile(`(?s)fetch\(\s*['"]([^'"]+)['"][^;]{0,800}?method:\s*['"](POST|PUT|PATCH|DELETE)['"]`)
	var files []string
	for _, root := range []string{"templates", filepath.Join("assets", "js")} {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && (strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".js")) {
				files = append(files, p)
			}
			return nil
		})
	}
	checked := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, m := range mutating.FindAllStringSubmatchIndex(src, -1) {
			path := src[m[2]:m[3]]
			if strings.HasPrefix(path, "/api/push/") {
				continue
			}
			checked++
			end := m[1] + 400
			if end > len(src) {
				end = len(src)
			}
			if !strings.Contains(src[m[0]:end], "X-CSRF-Token") {
				t.Errorf("%s: fetch(%q) is a mutating request without an X-CSRF-Token header", f, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no mutating fetch() calls; the scan regexp is stale")
	}
}

func TestAppJSExposesCSRFToken(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("assets", "js", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "lmCsrfToken") {
		t.Error("app.js must define window.lmCsrfToken for raw fetch() callers")
	}
}
