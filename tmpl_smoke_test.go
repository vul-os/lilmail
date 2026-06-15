package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/template/html/v2"
)

// TestTemplatesParse ensures every template in ./templates parses with the
// same custom funcs registered in main(). go build does not compile templates,
// so this guards against unbalanced {{end}} / unknown functions at runtime.
func TestTemplatesParse(t *testing.T) {
	engine := html.New("./templates", ".html")
	engine.AddFunc("split", strings.Split)
	engine.AddFunc("join", strings.Join)
	engine.AddFunc("lower", strings.ToLower)
	engine.AddFunc("upper", strings.ToUpper)
	engine.AddFunc("title", strings.Title)
	engine.AddFunc("trim", strings.TrimSpace)
	engine.AddFunc("hasPrefix", strings.HasPrefix)
	engine.AddFunc("urlEncode", func(s string) string { return s })
	engine.AddFunc("formatDate", func(tm time.Time) string { return tm.String() })
	engine.AddFunc("formatSize", func(n int) string { return fmt.Sprintf("%d", n) })
	engine.AddFunc("initial", func(name, email string) string { return "?" })
	engine.AddFunc("caldavEnabled", func() bool { return false })
	engine.AddFunc("notificationsEnabled", func() bool { return false })
	engine.AddFunc("webPushEnabled", func() bool { return false })
	engine.AddFunc("accountsEnabled", func() bool { return false })
	if err := engine.Load(); err != nil {
		t.Fatalf("template parse error: %v", err)
	}
}
