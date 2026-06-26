// handlers/web/push.go
//
// HTTP handlers for VAPID Web Push subscription management.
//
//	GET    /api/push/vapid-public  → returns {"publicKey":"<base64url>"}
//	POST   /api/push/subscribe     → upserts a PushSubscription; body = browser JSON
//	DELETE /api/push/subscribe     → removes a subscription by endpoint
//
// Push delivery is triggered from NotificationHub.Broadcast via SendPush.
package web

import (
	"encoding/json"
	"log"
	"net/http"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/gofiber/fiber/v2"
)

// PushHandler exposes VAPID public key and subscription management endpoints.
type PushHandler struct {
	keys  *VAPIDKeys
	store *PushStore
}

// NewPushHandler creates a handler. keys and store must be non-nil.
func NewPushHandler(keys *VAPIDKeys, store *PushStore) *PushHandler {
	return &PushHandler{keys: keys, store: store}
}

// HandleVAPIDPublicKey returns the VAPID public key so the browser can call
// PushManager.subscribe({ applicationServerKey: <key> }).
func (h *PushHandler) HandleVAPIDPublicKey(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"publicKey": h.keys.Public})
}

// HandleSubscribe upserts a Web Push subscription for the authenticated user.
// Body must be the JSON serialisation of a PushSubscription object as produced
// by the browser's pushSubscription.toJSON().
func (h *PushHandler) HandleSubscribe(c *fiber.Ctx) error {
	username, _ := c.Locals("username").(string)
	if username == "" {
		return fiber.ErrUnauthorized
	}

	var sub PushSubscription
	if err := c.BodyParser(&sub); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid subscription body")
	}
	if sub.Endpoint == "" || sub.Keys.P256DH == "" || sub.Keys.Auth == "" {
		return fiber.NewError(fiber.StatusBadRequest, "subscription missing required fields")
	}

	if err := h.store.Save(username, sub); err != nil {
		log.Printf("push: save subscription for %s: %v", username, err)
		return fiber.ErrInternalServerError
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"ok": true})
}

// HandleUnsubscribe removes a subscription by endpoint URL.
// Body: {"endpoint":"<url>"}
func (h *PushHandler) HandleUnsubscribe(c *fiber.Ctx) error {
	username, _ := c.Locals("username").(string)
	if username == "" {
		return fiber.ErrUnauthorized
	}

	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := c.BodyParser(&body); err != nil || body.Endpoint == "" {
		return fiber.NewError(fiber.StatusBadRequest, "endpoint required")
	}

	if err := h.store.Delete(username, body.Endpoint); err != nil {
		log.Printf("push: delete subscription for %s: %v", username, err)
		return fiber.ErrInternalServerError
	}
	return c.JSON(fiber.Map{"ok": true})
}

// SendPush delivers a push notification to every registered subscription for
// username. Expired/invalid subscriptions (HTTP 410 Gone) are removed from the
// store automatically.  This is called from NotificationHub.Broadcast.
func SendPush(username string, ev MailEvent, keys *VAPIDKeys, store *PushStore) {
	subs, err := store.All(username)
	if err != nil {
		log.Printf("push: list subscriptions for %s: %v", username, err)
		return
	}
	if len(subs) == 0 {
		return
	}

	payload := buildPushPayload(ev)

	for _, sub := range subs {
		resp, err := webpush.SendNotification(payload, toWebpushSub(sub), &webpush.Options{
			VAPIDPublicKey:  keys.Public,
			VAPIDPrivateKey: keys.Private,
			TTL:             300, // 5 minutes
		})
		if err != nil {
			log.Printf("push: send to %s endpoint %s: %v", username, sub.Endpoint, err)
			continue
		}
		resp.Body.Close()
		// RFC 8030 §8.3 — 410 Gone means the subscription is no longer valid.
		if resp.StatusCode == http.StatusGone {
			log.Printf("push: subscription gone for %s, removing endpoint %s", username, sub.Endpoint)
			if delErr := store.Delete(username, sub.Endpoint); delErr != nil {
				log.Printf("push: delete gone subscription: %v", delErr)
			}
		}
	}
}

// buildPushPayload converts a MailEvent to the JSON bytes sent as the push
// notification payload.  The service worker receives this verbatim.
// Payload must be ≤ 4 KB (RFC 8030).
func buildPushPayload(ev MailEvent) []byte {
	type payload struct {
		From    string `json:"from"`
		Subject string `json:"subject"`
		Tag     string `json:"tag"`
	}
	raw, _ := json.Marshal(payload{From: ev.From, Subject: ev.Subject, Tag: "newmail"})
	return raw
}

// toWebpushSub converts our PushSubscription to the webpush-go type.
func toWebpushSub(sub PushSubscription) *webpush.Subscription {
	return &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpush.Keys{
			P256dh: sub.Keys.P256DH,
			Auth:   sub.Keys.Auth,
		},
	}
}
