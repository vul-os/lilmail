// handlers/web/desktop_notify.go
//
// Thin wrapper around gen2brain/beeep for native OS desktop notifications.
// Only called when notifications.desktop = true in config.toml.
package web

import "github.com/gen2brain/beeep"

// notifyDesktop sends a native OS toast notification (macOS / Linux / Windows).
// icon is optional; pass "" to use no icon.
// Errors are silently swallowed so a notification failure never crashes the hub.
func notifyDesktop(title, body string) {
	_ = beeep.Notify(title, body, "")
}
