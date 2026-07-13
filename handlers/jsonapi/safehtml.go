// handlers/jsonapi/safehtml.go — conservative HTML sanitizer for user-authored
// snippets that lilmail EMITS into outgoing mail: signatures and vacation-reply
// bodies.
//
// The sanitizer policy now lives in the shared, dependency-free
// handlers/htmlsafe package so it can also defang the HTML of a DRAFT being
// edited before it enters the compose contenteditable in handlers/web (that
// contenteditable is assigned via innerHTML in the MAIN app document — see
// templates/partials/email-viewer.html restoreDraft() — which, unlike the
// sandboxed reading-pane iframe, would otherwise be a stored-XSS sink).
//
// WHY DEFANG THESE SNIPPETS: they are composed into an outgoing MIME text/html
// part read by the RECIPIENT's mail client, which applies its own strict
// mail-HTML sanitization. Our job here is defense-in-depth: strip the active
// content that has no place in a signature/auto-reply (script, event handlers,
// javascript:/vbscript:/data: URLs, <style>, <iframe>, forms) so a stored XSS
// payload cannot ride out on our envelope, and so the mail-ui can safely preview
// the snippet.
//
// SEPARATELY, validateHeaderValue (wave-49) still guards every HEADER these
// features touch (vacation Subject, send-as From) against CR/LF/NUL injection —
// this file guards the BODY.
package jsonapi

import "lilmail/handlers/htmlsafe"

// sanitizeSnippetHTML returns a defanged copy of user-authored signature/vacation
// HTML. It delegates to htmlsafe.SanitizeSnippet (the shared policy). Kept as a
// package-local alias so existing callers and tests are unchanged.
func sanitizeSnippetHTML(in string) string {
	return htmlsafe.SanitizeSnippet(in)
}
