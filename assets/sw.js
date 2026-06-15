// sw.js — LilMail Service Worker
//
// Responsibilities:
//   1. Handle 'push' events: decrypt the JSON payload and show a notification.
//   2. Handle 'notificationclick': focus an existing LilMail tab or open a new one.
//   3. Handle 'pushsubscriptionchange': re-subscribe and POST the new subscription.
//
// This file is intentionally kept minimal — no caching / offline logic.
// It is registered by the inline script in layouts/main.html when Web Push is
// enabled (webPushEnabled flag injected by the template).

'use strict';

const LILMAIL_ORIGIN = self.location.origin;

// ── Push event ──────────────────────────────────────────────────────────────

self.addEventListener('push', function (event) {
    let data = { from: 'Unknown sender', subject: '(no subject)', tag: 'newmail' };
    if (event.data) {
        try { data = event.data.json(); } catch (_) { /* ignore */ }
    }

    const title = 'New mail from ' + (data.from || 'unknown');
    const options = {
        body: data.subject || '(no subject)',
        icon: '/assets/icon.png',
        badge: '/assets/icon.png',
        tag: data.tag || 'newmail',          // collapse multiple notifications
        renotify: false,
        data: { url: LILMAIL_ORIGIN + '/inbox' },
    };

    event.waitUntil(self.registration.showNotification(title, options));
});

// ── Notification click ───────────────────────────────────────────────────────

self.addEventListener('notificationclick', function (event) {
    event.notification.close();
    const targetUrl = (event.notification.data && event.notification.data.url)
        ? event.notification.data.url
        : LILMAIL_ORIGIN + '/inbox';

    event.waitUntil(
        clients.matchAll({ type: 'window', includeUncontrolled: true }).then(function (windowClients) {
            // Focus an existing LilMail tab if one is open.
            for (var i = 0; i < windowClients.length; i++) {
                var client = windowClients[i];
                if (client.url.startsWith(LILMAIL_ORIGIN) && 'focus' in client) {
                    return client.focus();
                }
            }
            // Otherwise open a new tab.
            if (clients.openWindow) {
                return clients.openWindow(targetUrl);
            }
        })
    );
});

// ── Subscription change ──────────────────────────────────────────────────────
// Fired by the browser when a push subscription is automatically refreshed
// (e.g. after a browser update).  We re-subscribe and POST the new subscription
// to the server so push delivery isn't interrupted.

self.addEventListener('pushsubscriptionchange', function (event) {
    event.waitUntil(
        self.registration.pushManager.subscribe(event.oldSubscription.options)
            .then(function (newSub) {
                return fetch('/api/push/subscribe', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(newSub.toJSON()),
                });
            })
    );
});
