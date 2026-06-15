// handlers/web/notifications.go
//
// Phase-6 Notifications & real-time: SSE hub + IMAP IDLE watcher integration.
//
// Architecture
// ============
//   - NotificationHub  — singleton owned by main.go; created and registered
//     only when notifications.enabled = true.
//   - Per-user watcher goroutine — started on the first SSE subscriber for a
//     given username; stopped when the last subscriber disconnects.  It opens a
//     dedicated IMAP connection via AuthHandler.CreateIMAPClient, selects INBOX,
//     and calls api.Client.WatchInbox.
//   - SSE handler (GET /events) — each browser tab that connects registers a
//     buffered channel; the hub fan-outs new-mail events to all channels for
//     that user.
//
// Goroutine lifecycle
// ===================
//
//	subscribe   → hub.Subscribe(username, ch) → if first subscriber, start watcher
//	unsubscribe → hub.Unsubscribe(username, ch) → if last subscriber, close stop-chan
//	watcher exits → clean; next subscribe will restart it
//
// TODO(webpush): VAPID + Service Worker for background notifications (Web Push).
// See ROADMAP.md "Web Push (VAPID + Service Worker)".
package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"

	"lilmail/config"
	"lilmail/models"
)

// MailEvent is the JSON payload sent over SSE.
type MailEvent struct {
	From    string `json:"from"`
	Subject string `json:"subject"`
}

// userState holds the per-user subscriber list and the watcher's stop channel.
type userState struct {
	subs []chan MailEvent
	stop chan struct{} // non-nil when a watcher goroutine is running
}

// NotificationHub manages SSE subscriptions and per-user IMAP IDLE watchers.
type NotificationHub struct {
	mu        sync.Mutex
	users     map[string]*userState
	config    *config.Config
	store     *session.Store
	auth      *AuthHandler
	// Optional Web Push fields — nil when webpush is disabled.
	vapidKeys *VAPIDKeys
	pushStore *PushStore
}

// NewNotificationHub creates a hub ready to accept SSE subscribers.
// auth must be non-nil; store and cfg are forwarded to the watcher goroutine.
// vapidKeys and pushStore may be nil when Web Push is disabled.
func NewNotificationHub(store *session.Store, cfg *config.Config, auth *AuthHandler, vapidKeys *VAPIDKeys, pushStore *PushStore) *NotificationHub {
	return &NotificationHub{
		users:     make(map[string]*userState),
		config:    cfg,
		store:     store,
		auth:      auth,
		vapidKeys: vapidKeys,
		pushStore: pushStore,
	}
}

// Subscribe registers ch as a receiver of new-mail events for username.
// If no watcher is running for that user one is started immediately.
// The supplied fiberCtx is used once (inside startWatcher) to create the
// dedicated IMAP client from the live session; after the function returns
// the watcher goroutine no longer needs the fiber context.
func (h *NotificationHub) Subscribe(username string, ch chan MailEvent, fiberCtx *fiber.Ctx) {
	h.mu.Lock()
	defer h.mu.Unlock()

	st := h.users[username]
	if st == nil {
		st = &userState{}
		h.users[username] = st
	}
	st.subs = append(st.subs, ch)

	// Start a watcher if none is running.
	if st.stop == nil && h.config.Notifications.Idle {
		stop := make(chan struct{})
		st.stop = stop
		go h.runWatcher(username, stop, fiberCtx)
	}
}

// Unsubscribe removes ch from the subscriber list for username.
// When the last subscriber leaves the watcher goroutine is signalled to stop.
func (h *NotificationHub) Unsubscribe(username string, ch chan MailEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	st := h.users[username]
	if st == nil {
		return
	}

	// Remove ch from the slice.
	filtered := st.subs[:0]
	for _, s := range st.subs {
		if s != ch {
			filtered = append(filtered, s)
		}
	}
	st.subs = filtered

	// If nobody is left, stop the watcher.
	if len(st.subs) == 0 && st.stop != nil {
		close(st.stop)
		st.stop = nil
	}
	if len(st.subs) == 0 {
		delete(h.users, username)
	}
}

// Broadcast delivers an event to every active subscriber for username.
func (h *NotificationHub) Broadcast(username string, ev MailEvent) {
	h.mu.Lock()
	st := h.users[username]
	if st == nil {
		h.mu.Unlock()
		return
	}
	// Copy the slice so we don't hold the lock while writing.
	channels := make([]chan MailEvent, len(st.subs))
	copy(channels, st.subs)
	h.mu.Unlock()

	for _, ch := range channels {
		select {
		case ch <- ev:
		default:
			// Subscriber is too slow; skip rather than block.
		}
	}

	// Native desktop notification (opt-in).
	if h.config.Notifications.Desktop {
		notifyDesktop("New mail from "+ev.From, ev.Subject)
	}

	// Web Push (opt-in, background — works even when no browser tab is open).
	if h.config.Notifications.WebPush && h.vapidKeys != nil && h.pushStore != nil {
		go SendPush(username, ev, h.vapidKeys, h.pushStore)
	}
}

// runWatcher opens a dedicated IMAP connection for username and calls
// api.Client.WatchInbox until stop is closed or the connection drops.
// fiberCtx is used once to obtain session credentials via CreateIMAPClient.
func (h *NotificationHub) runWatcher(username string, stop <-chan struct{}, fiberCtx *fiber.Ctx) {
	imapClient, err := h.auth.CreateIMAPClient(fiberCtx)
	if err != nil {
		log.Printf("notifications: watcher for %s: create IMAP client: %v", username, err)
		// Clear the stop channel so a future subscription can retry.
		h.mu.Lock()
		if st := h.users[username]; st != nil {
			st.stop = nil
		}
		h.mu.Unlock()
		return
	}
	defer func() {
		if err := imapClient.Close(); err != nil {
			log.Printf("notifications: watcher for %s: close: %v", username, err)
		}
	}()

	err = imapClient.WatchInbox(stop, func(email models.Email) {
		h.Broadcast(username, MailEvent{
			From:    email.From,
			Subject: email.Subject,
		})
	})
	if err != nil {
		log.Printf("notifications: watcher for %s stopped: %v", username, err)
	}

	// Clean up the stop reference so next subscriber restarts the watcher.
	h.mu.Lock()
	if st := h.users[username]; st != nil {
		st.stop = nil
	}
	h.mu.Unlock()
}

// NotificationsHandler wraps the hub and implements the SSE HTTP handler.
type NotificationsHandler struct {
	hub *NotificationHub
}

// NewNotificationsHandler wires up the hub.
func NewNotificationsHandler(hub *NotificationHub) *NotificationsHandler {
	return &NotificationsHandler{hub: hub}
}

// HandleSSE is the Fiber handler for GET /events.
// It streams Server-Sent Events to the browser for as long as the connection
// stays open, forwarding new-mail notifications from the hub.
func (nh *NotificationsHandler) HandleSSE(c *fiber.Ctx) error {
	username, _ := c.Locals("username").(string)
	if username == "" {
		return fiber.ErrUnauthorized
	}

	// Buffered so the hub's Broadcast never blocks waiting for this client.
	ch := make(chan MailEvent, 8)
	nh.hub.Subscribe(username, ch, c)

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no") // Tell nginx not to buffer SSE.

	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer nh.hub.Unsubscribe(username, ch)

		// Send a comment heartbeat immediately so the browser knows we're live.
		fmt.Fprintf(w, ": connected\n\n")
		w.Flush() //nolint:errcheck

		for ev := range ch {
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if err := w.Flush(); err != nil {
				// Client disconnected.
				return
			}
		}
	})

	return nil
}
