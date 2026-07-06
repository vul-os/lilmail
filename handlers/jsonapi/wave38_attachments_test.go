// handlers/jsonapi/wave38_attachments_test.go — regression coverage for the
// "mail attachments seem broken" report (wave 38).
//
// Root cause was a config/deploy asymmetry: outbound attachment STAGING needs a
// cache folder, but config.Cache.Folder had no default, so a deployment whose
// config.toml omitted [cache] folder returned 503 on every upload while
// downloads (which don't need staging) kept working — i.e. you could open
// received attachments but never attach one when composing. These tests lock in
// two behaviours: (1) an unconfigured staging dir degrades to a CLEAN, probeable
// 503 (never a confusing 500), and (2) a configured one accepts the upload.
package jsonapi

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"testing"

	"lilmail/config"

	"github.com/gofiber/fiber/v2"
)

// uploadOneFile POSTs a small multipart file to /v1/attachments through the
// brokered app and returns the response.
func uploadOneFile(t *testing.T, app *fiber.App) (*httptest.ResponseRecorder, int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "hello.txt")
	_, _ = fw.Write([]byte("attachment-contents"))
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/v1/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	return nil, resp.StatusCode, b
}

// With NO cache folder configured, upload staging is unavailable. It must return
// a clean 503 with a JSON {"error":...} the UI can degrade on — NOT a 500 that
// reads as "the server is broken".
func TestUploadStagingUnconfiguredDegradesTo503(t *testing.T) {
	cfg := &config.Config{} // Cache.Folder deliberately empty
	app := newBrokeredAppCfg(t, cfg, &fakeMailClient{})

	_, status, body := uploadOneFile(t, app)
	if status != fiber.StatusServiceUnavailable {
		t.Fatalf("unconfigured staging: status = %d, want 503; body=%s", status, body)
	}
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Error == "" {
		t.Fatalf("503 body must be a JSON {error}: %s (err=%v)", body, err)
	}
}

// With a cache folder configured, the same upload succeeds (201 + token). This
// is the other half of the degrade contract — the feature works when the deploy
// is configured correctly (as the default ./cache now makes it out of the box).
func TestUploadStagingConfiguredSucceeds(t *testing.T) {
	cfg := &config.Config{}
	cfg.Cache.Folder = t.TempDir()
	app := newBrokeredAppCfg(t, cfg, &fakeMailClient{})

	_, status, body := uploadOneFile(t, app)
	if status != fiber.StatusCreated {
		t.Fatalf("configured staging: status = %d, want 201; body=%s", status, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		t.Fatalf("201 body must carry a token: %s (err=%v)", body, err)
	}
}
