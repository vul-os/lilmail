// handlers/jsonapi/compose_attachments.go — attachment staging for JSON compose.
//
// The compose/send path (POST /v1/messages and /v1/drafts) accepts a JSON body,
// but a rich client needs to attach files whose bytes don't belong inline in
// that JSON. This file adds a two-step, stateless-friendly upload flow:
//
//  1. POST /v1/attachments  (multipart form, field "file")
//     → validates + stages the bytes under the caller's cache namespace,
//     returns {token, filename, size, contentType}.
//  2. POST /v1/messages  body {... "attachments":[{"token":"..."}]}
//     → each staged token is resolved to an api.OutgoingAttachment, threaded
//     into the SAME api.BuildMIMEMessage engine, then consumed (deleted).
//
// A client may also attach INLINE without step 1 by sending
// {"filename","contentType","data"(base64)} in the attachments array — handy for
// small files and fully stateless. Both forms funnel through resolveAttachments.
//
// Staging is per-account (namespaced by the sanitized sender email, same scheme
// the rest of lilmail uses for cache dirs) under config.Cache.Folder, so one
// account can never read another's staged uploads, and it works identically in
// session and CP-brokered modes on a single instance. Tokens are 128-bit random
// hex and path-validated, so an attacker cannot traverse out of the staging dir.
package jsonapi

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"lilmail/handlers/api"

	"github.com/gofiber/fiber/v2"
)

// maxComposeAttachmentBytes caps a single staged/inline attachment. It mirrors
// the download cap and stays within the app's 25 MiB BodyLimit.
const maxComposeAttachmentBytes = 25 * 1024 * 1024

// stagedTTL is how long a staged upload survives before opportunistic cleanup
// reclaims it (a compose that is never sent should not leak disk forever).
const stagedTTL = 24 * time.Hour

// tokenRe matches the exact shape of a token minted by newStageToken: 32 lower
// hex chars (128 bits). Anything else is rejected before it touches the FS, so a
// caller-supplied token can never contain path separators or ".." traversal.
var tokenRe = regexp.MustCompile(`^[a-f0-9]{32}$`)

// attachmentRef is one entry of the compose body's "attachments" array. Exactly
// one of Token (a prior /v1/attachments upload) or Data (inline base64) is used;
// Filename/ContentType override the staged metadata when set.
type attachmentRef struct {
	Token       string `json:"token"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Data        string `json:"data"` // base64-encoded bytes for inline attachments
}

// stagedMeta is the sidecar JSON stored next to a staged blob.
type stagedMeta struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int    `json:"size"`
	CreatedUnix int64  `json:"created"`
}

// stagingDir returns (and creates) the per-account staging directory. It returns
// an error when no cache folder is configured — staging is unavailable then.
func (h *Handler) stagingDir(c *fiber.Ctx) (string, error) {
	if strings.TrimSpace(h.config.Cache.Folder) == "" {
		return "", os.ErrNotExist
	}
	dir := filepath.Join(h.config.Cache.Folder, api.SanitizeUsername(h.fromEmail(c)), "compose-staging")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// newStageToken returns a 128-bit random lower-hex token.
func newStageToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// handleUploadAttachment stages one multipart file for a later compose/send.
// POST /v1/attachments  multipart form, file field "file"
// → 201 {token, filename, size, contentType}
func (h *Handler) handleUploadAttachment(c *fiber.Ctx) error {
	fh, err := c.FormFile("file")
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "multipart file field \"file\" is required")
	}
	if fh.Size <= 0 {
		return fail(c, fiber.StatusBadRequest, "empty file")
	}
	if fh.Size > maxComposeAttachmentBytes {
		return fail(c, fiber.StatusRequestEntityTooLarge, "attachment exceeds maximum allowed size")
	}

	dir, err := h.stagingDir(c)
	if err != nil {
		return fail(c, fiber.StatusServiceUnavailable, "attachment staging unavailable")
	}
	sweepStaging(dir) // opportunistic GC of abandoned uploads

	token, err := newStageToken()
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not allocate upload token")
	}

	src, err := fh.Open()
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "could not read uploaded file")
	}
	defer src.Close()

	blobPath := filepath.Join(dir, token+".bin")
	dst, err := os.OpenFile(blobPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail(c, fiber.StatusInternalServerError, "could not stage upload")
	}
	written, copyErr := copyLimited(dst, src, maxComposeAttachmentBytes)
	closeErr := dst.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(blobPath)
		return fail(c, fiber.StatusInternalServerError, "could not stage upload")
	}

	filename := filepath.Base(fh.Filename)
	if filename == "" || filename == "." || filename == "/" {
		filename = "attachment"
	}
	contentType := fh.Header.Get("Content-Type")

	meta := stagedMeta{Filename: filename, ContentType: contentType, Size: int(written), CreatedUnix: time.Now().Unix()}
	metaBytes, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, token+".json"), metaBytes, 0o600); err != nil {
		os.Remove(blobPath)
		return fail(c, fiber.StatusInternalServerError, "could not stage upload metadata")
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"token":       token,
		"filename":    filename,
		"size":        written,
		"contentType": contentType,
	})
}

// resolveAttachments turns the compose body's attachment refs into ready-to-send
// api.OutgoingAttachment values. Staged tokens are read from disk and CONSUMED
// (deleted) so a token cannot be replayed; inline base64 refs are decoded in
// place. A total-size cap guards against a single request assembling a huge
// message. On any error nothing is sent and the caller returns 4xx/5xx.
func (h *Handler) resolveAttachments(c *fiber.Ctx, refs []attachmentRef) ([]api.OutgoingAttachment, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	var (
		out   []api.OutgoingAttachment
		total int
	)
	dir, dirErr := h.stagingDir(c)

	for _, ref := range refs {
		var (
			data     []byte
			filename = ref.Filename
			ctype    = ref.ContentType
		)

		switch {
		case ref.Token != "":
			if dirErr != nil {
				return nil, fiber.NewError(fiber.StatusServiceUnavailable, "attachment staging unavailable")
			}
			if !tokenRe.MatchString(ref.Token) {
				return nil, fiber.NewError(fiber.StatusBadRequest, "invalid attachment token")
			}
			blob, meta, err := readStaged(dir, ref.Token)
			if err != nil {
				return nil, fiber.NewError(fiber.StatusBadRequest, "unknown or expired attachment token")
			}
			data = blob
			if filename == "" {
				filename = meta.Filename
			}
			if ctype == "" {
				ctype = meta.ContentType
			}
			consumeStaged(dir, ref.Token) // one-shot: prevent replay + reclaim disk

		case ref.Data != "":
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ref.Data))
			if err != nil {
				return nil, fiber.NewError(fiber.StatusBadRequest, "attachment data is not valid base64")
			}
			data = decoded

		default:
			return nil, fiber.NewError(fiber.StatusBadRequest, "each attachment needs a token or base64 data")
		}

		if len(data) == 0 {
			return nil, fiber.NewError(fiber.StatusBadRequest, "empty attachment")
		}
		total += len(data)
		if len(data) > maxComposeAttachmentBytes || total > maxComposeAttachmentBytes {
			return nil, fiber.NewError(fiber.StatusRequestEntityTooLarge, "attachments exceed maximum allowed size")
		}
		if filename == "" {
			filename = "attachment"
		}
		out = append(out, api.OutgoingAttachment{Filename: filename, ContentType: ctype, Data: data})
	}
	return out, nil
}

// copyLimited copies at most max bytes from src to dst, erroring if the source
// is longer (defence-in-depth beyond the pre-checked multipart size).
func copyLimited(dst io.Writer, src io.Reader, max int64) (int64, error) {
	n, err := io.Copy(dst, io.LimitReader(src, max+1))
	if err != nil {
		return n, err
	}
	if n > max {
		return n, io.ErrShortWrite
	}
	return n, nil
}

// readStaged loads a staged blob + its metadata by token.
func readStaged(dir, token string) ([]byte, stagedMeta, error) {
	var meta stagedMeta
	metaBytes, err := os.ReadFile(filepath.Join(dir, token+".json"))
	if err != nil {
		return nil, meta, err
	}
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, meta, err
	}
	blob, err := os.ReadFile(filepath.Join(dir, token+".bin"))
	if err != nil {
		return nil, meta, err
	}
	return blob, meta, nil
}

// consumeStaged removes a staged blob + metadata (best effort).
func consumeStaged(dir, token string) {
	os.Remove(filepath.Join(dir, token+".bin"))
	os.Remove(filepath.Join(dir, token+".json"))
}

// sweepStaging deletes staged files older than stagedTTL (best effort GC).
func sweepStaging(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-stagedTTL)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
