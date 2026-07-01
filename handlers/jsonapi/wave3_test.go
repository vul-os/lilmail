package jsonapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lilmail/config"
	"lilmail/handlers/api"
	"lilmail/handlers/web"
	"lilmail/models"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

// recordClient records the paging + flag calls the handlers make, so the /v1
// layer's behaviour can be asserted without a live IMAP server.
type recordClient struct {
	*fakeMailClient
	pagedFolder             string
	pagedLimit, pagedOffset uint32
	pagedReturn             []models.Email

	flagCalls []flagCall
}

type flagCall struct {
	folder, uid, flag string
	add               bool
}

func (r *recordClient) FetchMessagesPaged(folder string, limit, offset uint32) ([]models.Email, error) {
	r.pagedFolder, r.pagedLimit, r.pagedOffset = folder, limit, offset
	return r.pagedReturn, nil
}

func (r *recordClient) SetMessageFlag(folder, uid, flag string, add bool) error {
	r.flagCalls = append(r.flagCalls, flagCall{folder, uid, flag, add})
	return nil
}

// captureSMTP records the raw message + recipients handed to the send path.
type captureSMTP struct {
	rcpts []string
	raw   []byte
}

func (c *captureSMTP) SendRawMessage(rcpts []string, raw []byte) error {
	c.rcpts, c.raw = rcpts, raw
	return nil
}

// newBrokeredAppCfg is like newBrokeredApp but takes an explicit config (so a
// test can set Cache.Folder for the attachment-staging path). It wires a /v1 app
// in CP-brokered mode to the given fake mail client.
func newBrokeredAppCfg(t *testing.T, cfg *config.Config, cl api.MailClient) *fiber.App {
	t.Helper()
	t.Setenv(brokerEnvSecret, "s3cr3t")
	orig := brokerDialIMAP
	brokerDialIMAP = func(brokerSpec) (api.MailClient, error) { return cl, nil }
	t.Cleanup(func() { brokerDialIMAP = orig })

	store := session.New()
	h := New(store, cfg, web.NewAuthHandler(store, cfg))
	app := fiber.New()
	h.Register(app)
	return app
}

// --- Pagination -------------------------------------------------------------

// GET /v1/messages must thread ?offset through to FetchMessagesPaged and report
// the effective limit/offset + a nextOffset cursor.
func TestMessagesPaginationOffset(t *testing.T) {
	cfg := &config.Config{}
	rc := &recordClient{fakeMailClient: &fakeMailClient{}}
	// Return exactly `limit` messages so nextOffset advances.
	rc.pagedReturn = []models.Email{{ID: "10"}, {ID: "9"}}

	app := newBrokeredAppCfg(t, cfg, rc)

	req := httptest.NewRequest("GET", "/v1/messages?folder=INBOX&limit=2&offset=4", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, b)
	}
	if rc.pagedFolder != "INBOX" || rc.pagedLimit != 2 || rc.pagedOffset != 4 {
		t.Fatalf("paging args not threaded: folder=%q limit=%d offset=%d", rc.pagedFolder, rc.pagedLimit, rc.pagedOffset)
	}

	var out struct {
		Limit      uint32          `json:"limit"`
		Offset     uint32          `json:"offset"`
		NextOffset *uint32         `json:"nextOffset"`
		Messages   []models.Email  `json:"messages"`
		Folder     string          `json:"folder"`
		Raw        json.RawMessage `json:"-"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("bad JSON: %s", b)
	}
	if out.Limit != 2 || out.Offset != 4 {
		t.Fatalf("echoed limit/offset wrong: %+v", out)
	}
	if out.NextOffset == nil || *out.NextOffset != 6 {
		t.Fatalf("nextOffset = %v; want 6 (full page)", out.NextOffset)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages len = %d; want 2", len(out.Messages))
	}
}

// A short (partial) page signals end-of-mailbox: nextOffset must be null.
func TestMessagesPaginationEndOfMailbox(t *testing.T) {
	cfg := &config.Config{}
	rc := &recordClient{fakeMailClient: &fakeMailClient{}}
	rc.pagedReturn = []models.Email{{ID: "1"}} // fewer than limit

	app := newBrokeredAppCfg(t, cfg, rc)

	req := httptest.NewRequest("GET", "/v1/messages?limit=50&offset=0", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	resp, _ := app.Test(req)
	b, _ := io.ReadAll(resp.Body)

	var out struct {
		NextOffset *uint32 `json:"nextOffset"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("bad JSON: %s", b)
	}
	if out.NextOffset != nil {
		t.Fatalf("nextOffset = %v; want null at end of mailbox", *out.NextOffset)
	}
}

// FetchMessagesPaged on the demo client honours offset + limit windows.
func TestDemoClientPaging(t *testing.T) {
	d := api.NewDemoClient()
	page1, err := d.FetchMessagesPaged("INBOX", 2, 0)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	page2, err := d.FetchMessagesPaged("INBOX", 2, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page1) == 0 {
		t.Skip("demo inbox has no seed messages")
	}
	if len(page1) > 0 && len(page2) > 0 && page1[0].ID == page2[0].ID {
		t.Fatalf("offset did not advance the window: %q == %q", page1[0].ID, page2[0].ID)
	}
	// Offset past the end returns empty, never an error.
	end, err := d.FetchMessagesPaged("INBOX", 5, 1000)
	if err != nil || len(end) != 0 {
		t.Fatalf("offset past end: got %d msgs, err=%v", len(end), err)
	}
}

// --- Attachment upload + compose -------------------------------------------

// Full flow: upload a file to /v1/attachments, then send referencing the staged
// token; the assembled MIME must carry the attachment and the staging must be
// consumed (deleted) afterwards.
func TestAttachmentUploadThenSend(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.Folder = t.TempDir()

	fake := &fakeMailClient{}
	app := newBrokeredAppCfg(t, cfg, fake)

	cap := &captureSMTP{}
	origSMTP := brokerSMTPSender
	brokerSMTPSender = func(brokerSpec) smtpSender { return cap }
	t.Cleanup(func() { brokerSMTPSender = origSMTP })

	// 1. Upload.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "hello.txt")
	fw.Write([]byte("attachment-contents"))
	mw.Close()

	up := httptest.NewRequest("POST", "/v1/attachments", &buf)
	up.Header.Set("Content-Type", mw.FormDataContentType())
	up.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		up.Header.Set(k, v)
	}
	upResp, err := app.Test(up)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if upResp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(upResp.Body)
		t.Fatalf("upload want 201, got %d: %s", upResp.StatusCode, b)
	}
	var upOut struct {
		Token       string `json:"token"`
		Filename    string `json:"filename"`
		Size        int    `json:"size"`
		ContentType string `json:"contentType"`
	}
	b, _ := io.ReadAll(upResp.Body)
	if err := json.Unmarshal(b, &upOut); err != nil {
		t.Fatalf("upload JSON: %s", b)
	}
	if upOut.Token == "" || upOut.Filename != "hello.txt" || upOut.Size != len("attachment-contents") {
		t.Fatalf("upload response wrong: %+v", upOut)
	}

	// Staged files must exist on disk under the account namespace.
	stageDir := filepath.Join(cfg.Cache.Folder, api.SanitizeUsername("user@gmail.com"), "compose-staging")
	if _, err := os.Stat(filepath.Join(stageDir, upOut.Token+".bin")); err != nil {
		t.Fatalf("staged blob missing: %v", err)
	}

	// 2. Send referencing the token.
	sendBody, _ := json.Marshal(map[string]any{
		"to":      "dest@example.com",
		"subject": "hi",
		"text":    "see attached",
		"attachments": []map[string]string{
			{"token": upOut.Token},
		},
	})
	send := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(sendBody))
	send.Header.Set("Content-Type", "application/json")
	send.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		send.Header.Set(k, v)
	}
	sendResp, err := app.Test(send)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if sendResp.StatusCode != fiber.StatusCreated {
		bb, _ := io.ReadAll(sendResp.Body)
		t.Fatalf("send want 201, got %d: %s", sendResp.StatusCode, bb)
	}

	raw := string(cap.raw)
	if !strings.Contains(raw, "multipart/mixed") {
		t.Fatalf("sent message is not multipart/mixed:\n%s", raw)
	}
	if !strings.Contains(raw, `filename="hello.txt"`) {
		t.Fatalf("attachment filename missing from MIME:\n%s", raw)
	}
	wantB64 := base64.StdEncoding.EncodeToString([]byte("attachment-contents"))
	if !strings.Contains(raw, wantB64) {
		t.Fatalf("attachment bytes missing from MIME (want b64 %q)", wantB64)
	}

	// 3. Token consumed: staged files gone.
	if _, err := os.Stat(filepath.Join(stageDir, upOut.Token+".bin")); !os.IsNotExist(err) {
		t.Fatalf("staged blob not consumed after send: err=%v", err)
	}
}

// Inline base64 attachments require no upload step and must also be assembled.
func TestInlineBase64Attachment(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.Folder = t.TempDir()

	fake := &fakeMailClient{}
	app := newBrokeredAppCfg(t, cfg, fake)

	cap := &captureSMTP{}
	origSMTP := brokerSMTPSender
	brokerSMTPSender = func(brokerSpec) smtpSender { return cap }
	t.Cleanup(func() { brokerSMTPSender = origSMTP })

	payload := base64.StdEncoding.EncodeToString([]byte("PDFDATA"))
	sendBody, _ := json.Marshal(map[string]any{
		"to":      "dest@example.com",
		"subject": "inline",
		"text":    "body",
		"attachments": []map[string]string{
			{"filename": "doc.pdf", "contentType": "application/pdf", "data": payload},
		},
	})
	send := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(sendBody))
	send.Header.Set("Content-Type", "application/json")
	send.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		send.Header.Set(k, v)
	}
	resp, err := app.Test(send)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 201, got %d: %s", resp.StatusCode, b)
	}
	raw := string(cap.raw)
	if !strings.Contains(raw, `filename="doc.pdf"`) || !strings.Contains(raw, "application/pdf") {
		t.Fatalf("inline attachment not assembled:\n%s", raw)
	}
	if !strings.Contains(raw, payload) {
		t.Fatalf("inline attachment bytes missing")
	}
}

// A bogus/expired token must be rejected (never silently dropped).
func TestSendWithUnknownTokenRejected(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.Folder = t.TempDir()
	app := newBrokeredAppCfg(t, cfg, &fakeMailClient{})

	sendBody, _ := json.Marshal(map[string]any{
		"to":          "dest@example.com",
		"subject":     "x",
		"text":        "y",
		"attachments": []map[string]string{{"token": "deadbeefdeadbeefdeadbeefdeadbeef"}},
	})
	send := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(sendBody))
	send.Header.Set("Content-Type", "application/json")
	send.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		send.Header.Set(k, v)
	}
	resp, _ := app.Test(send)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400 for unknown token, got %d", resp.StatusCode)
	}
}

// A path-traversal token must be rejected before touching the filesystem.
func TestSendWithMalformedTokenRejected(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.Folder = t.TempDir()
	app := newBrokeredAppCfg(t, cfg, &fakeMailClient{})

	sendBody, _ := json.Marshal(map[string]any{
		"to":          "dest@example.com",
		"subject":     "x",
		"text":        "y",
		"attachments": []map[string]string{{"token": "../../etc/passwd"}},
	})
	send := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(sendBody))
	send.Header.Set("Content-Type", "application/json")
	send.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		send.Header.Set(k, v)
	}
	resp, _ := app.Test(send)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400 for malformed token, got %d", resp.StatusCode)
	}
}

// --- Labels / custom keywords ----------------------------------------------

// PATCH /v1/messages/:uid/flags must apply a batch of custom keywords (labels)
// from the client, and still accept the single-flag form.
func TestApplyLabelsFromClient(t *testing.T) {
	cfg := &config.Config{}
	rc := &recordClient{fakeMailClient: &fakeMailClient{}}
	app := newBrokeredAppCfg(t, cfg, rc)

	body, _ := json.Marshal(map[string]any{
		"flags": []string{"Important", "$Label1"},
		"add":   true,
	})
	req := httptest.NewRequest("PATCH", "/v1/messages/7/flags?folder=INBOX", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 204, got %d: %s", resp.StatusCode, b)
	}
	if len(rc.flagCalls) != 2 {
		t.Fatalf("want 2 flag STOREs, got %d: %+v", len(rc.flagCalls), rc.flagCalls)
	}
	got := map[string]bool{}
	for _, fc := range rc.flagCalls {
		if fc.folder != "INBOX" || fc.uid != "7" || !fc.add {
			t.Fatalf("flag call wrong: %+v", fc)
		}
		got[fc.flag] = true
	}
	if !got["Important"] || !got["$Label1"] {
		t.Fatalf("custom keywords not applied: %+v", rc.flagCalls)
	}
}

// The single-flag form is preserved for backward compatibility.
func TestApplySingleFlagBackwardCompat(t *testing.T) {
	cfg := &config.Config{}
	rc := &recordClient{fakeMailClient: &fakeMailClient{}}
	app := newBrokeredAppCfg(t, cfg, rc)

	body, _ := json.Marshal(map[string]any{"flag": `\Seen`, "add": true})
	req := httptest.NewRequest("PATCH", "/v1/messages/3/flags", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}
	if len(rc.flagCalls) != 1 || rc.flagCalls[0].flag != `\Seen` {
		t.Fatalf("single-flag form broken: %+v", rc.flagCalls)
	}
}
