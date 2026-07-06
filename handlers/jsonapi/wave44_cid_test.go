// handlers/jsonapi/wave44_cid_test.go — round-trip coverage for the /v1 inline
// (cid:) attachment shape added in wave 44.
//
// These tests exercise the real request path: a JSON compose body carrying an
// attachment ref with {"inline":true,"contentId":...,"data":...} is BodyParsed,
// resolved through resolveAttachments (the exact seam handleSend/handleSaveDraft
// use), and fed into api.BuildMIMEMessage — asserting the disposition/Content-ID
// survive end to end so vulos-mail-ui can switch inline paste from data: to cid:.
package jsonapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/handlers/web"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

// registerCIDProbe mounts a POST /v1/_cidprobe route that runs the same
// resolveAttachments → BuildMIMEMessage pipeline as the real send path and
// returns the raw MIME bytes. It lets a test assert the /v1 JSON shape maps onto
// a well-formed multipart/related without needing a live SMTP server.
func newCIDProbeApp(t *testing.T, cfg *config.Config) *fiber.App {
	t.Helper()
	t.Setenv(brokerEnvSecret, "s3cr3t")
	orig := brokerDialIMAP
	brokerDialIMAP = func(brokerSpec) (api.MailClient, error) { return &fakeMailClient{}, nil }
	t.Cleanup(func() { brokerDialIMAP = orig })

	store := session.New()
	h := New(store, cfg, web.NewAuthHandler(store, cfg))
	app := fiber.New()
	h.Register(app)

	// Mount the probe under the same broker-protected group so fromEmail/staging
	// resolve exactly as in production.
	grp := app.Group("/v1", h.brokerMiddleware, h.requireAuth)
	grp.Post("/_cidprobe", func(c *fiber.Ctx) error {
		var body composeBody
		if err := c.BodyParser(&body); err != nil {
			return fail(c, fiber.StatusBadRequest, "invalid JSON body")
		}
		plain := body.Text
		if plain == "" && body.HTML != "" {
			plain = stripHTMLForPlain(body.HTML)
		}
		atts, err := h.resolveAttachments(c, body.Attachments)
		if err != nil {
			return failErr(c, err)
		}
		raw, err := api.BuildMIMEMessage(api.MIMEMessageOptions{
			From:        h.fromEmail(c),
			To:          body.To,
			Subject:     body.Subject,
			PlainBody:   plain,
			HTMLBody:    body.HTML,
			Attachments: atts,
		})
		if err != nil {
			return fail(c, fiber.StatusInternalServerError, err.Error())
		}
		c.Set("Content-Type", "message/rfc822")
		return c.Send(raw)
	})
	return app
}

func postCIDProbe(t *testing.T, app *fiber.App, body composeBody) (int, []byte) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/v1/_cidprobe", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("probe request: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// The /v1 inline shape (inline:true + contentId + base64 data) round-trips into
// a multipart/related message with the inline image carrying the right
// Content-ID and inline disposition.
func TestV1InlineCIDRoundTrip(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.Folder = t.TempDir()
	app := newCIDProbeApp(t, cfg)

	status, raw := postCIDProbe(t, app, composeBody{
		To:      "bob@example.com",
		Subject: "Inline via v1",
		HTML:    `<p>Hello <img src="cid:pic1"></p>`,
		Attachments: []attachmentRef{{
			Filename:    "pic.png",
			ContentType: "image/png",
			Data:        base64.StdEncoding.EncodeToString([]byte("\x89PNG bytes")),
			ContentID:   "pic1",
			Inline:      true,
		}},
	})
	if status != fiber.StatusOK {
		t.Fatalf("probe status = %d: %s", status, raw)
	}

	mt, _, err := mime.ParseMediaType(topContentType(t, raw))
	if err != nil {
		t.Fatalf("parse top: %v", err)
	}
	if mt != "multipart/related" {
		t.Fatalf("top media type = %q, want multipart/related", mt)
	}
	// Find the inline image part.
	img := findRawPart(t, raw, "image/png")
	if img == nil {
		t.Fatal("inline image part not found in round-trip MIME")
	}
	if got := img.Get("Content-ID"); got != "<pic1>" {
		t.Errorf("Content-ID: got %q, want <pic1>", got)
	}
	if got := img.Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
		t.Errorf("Content-Disposition: got %q, want inline...", got)
	}
}

// An inline ref with no contentId is a clean 400 (not a 500 at build time).
func TestV1InlineWithoutContentIDRejected(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.Folder = t.TempDir()
	app := newCIDProbeApp(t, cfg)

	status, body := postCIDProbe(t, app, composeBody{
		To:      "bob@example.com",
		Subject: "Bad inline",
		HTML:    `<p><img src="cid:x"></p>`,
		Attachments: []attachmentRef{{
			ContentType: "image/png",
			Data:        base64.StdEncoding.EncodeToString([]byte("png")),
			Inline:      true, // no ContentID
		}},
	})
	if status != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, body)
	}
}

// A Content-ID carrying CRLF header-injection is rejected before any bytes are
// sent (BuildMIMEMessage validation surfaces as a 500 from the probe; the real
// send path returns "failed to build message").
func TestV1InlineContentIDInjectionRejected(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.Folder = t.TempDir()
	app := newCIDProbeApp(t, cfg)

	status, _ := postCIDProbe(t, app, composeBody{
		To:      "bob@example.com",
		Subject: "Evil",
		HTML:    `<p><img src="cid:x"></p>`,
		Attachments: []attachmentRef{{
			ContentType: "image/png",
			Data:        base64.StdEncoding.EncodeToString([]byte("png")),
			ContentID:   "x\r\nBcc: attacker@evil.example",
			Inline:      true,
		}},
	})
	if status == fiber.StatusOK {
		t.Fatal("injected Content-ID must not produce a 200 message")
	}
}

// --- helpers ---------------------------------------------------------------

func topContentType(t *testing.T, raw []byte) string {
	t.Helper()
	idx := bytes.Index(raw, []byte("\r\n\r\n"))
	if idx < 0 {
		t.Fatal("no header/body separator")
	}
	for _, line := range strings.Split(string(raw[:idx]), "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "content-type:") {
			return strings.TrimSpace(line[len("content-type:"):])
		}
	}
	t.Fatal("no Content-Type header")
	return ""
}

// findRawPart recursively searches a raw RFC822 message for the first leaf part
// whose media type matches want, returning its MIME header.
func findRawPart(t *testing.T, raw []byte, want string) textproto.MIMEHeader {
	t.Helper()
	ct := topContentType(t, raw)
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil
	}
	idx := bytes.Index(raw, []byte("\r\n\r\n"))
	body := raw[idx+4:]
	return searchParts(t, mt, params, body, want)
}

func searchParts(t *testing.T, mt string, params map[string]string, body []byte, want string) textproto.MIMEHeader {
	t.Helper()
	if !strings.HasPrefix(mt, "multipart/") {
		return nil
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			return nil
		}
		pb, _ := io.ReadAll(p)
		pmt, pparams, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
		if err != nil {
			continue
		}
		if pmt == want {
			return p.Header
		}
		if strings.HasPrefix(pmt, "multipart/") {
			if h := searchParts(t, pmt, pparams, pb, want); h != nil {
				return h
			}
		}
	}
}
