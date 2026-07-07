// handlers/jsonapi/parity_security_test.go — adversarial regression tests for the
// /v1 Gmail-parity surfaces (vacation, signatures, identities, connected accounts,
// unified inbox). Each test pins a CONFIRMED gap found in the security review so it
// cannot regress:
//
//   - sanitizer bypasses: slash-separated event handlers, entity-obfuscated and
//     tab-split javascript: schemes — the sanitized output feeds an OUTBOUND
//     auto-reply / signature, so a live handler here is a real stored-XSS carrier;
//   - the "|" key-delimiter cross-owner isolation hole (an owner whose identity
//     contains "|" could enumerate a neighbouring owner's records via the list
//     prefix scan);
//   - the vacation-body → outbound-reply sanitization contract end to end;
//   - the unified per-account fetch-size clamp (hostile ?limit amplification);
//   - the credential-never-in-response / send-as-cannot-spoof soundness checks.
package jsonapi

import (
	"encoding/json"
	"strings"
	"testing"

	"lilmail/handlers/api"
	"lilmail/models"

	"github.com/gofiber/fiber/v2"
)

// --- Sanitizer: outbound-reply / signature XSS carriers ----------------------

// TestSanitizerNeutralisesEventHandlerSeparators pins that an event handler is
// stripped regardless of the attribute separator the attacker uses. HTML treats
// "<a/onclick=...>" identically to "<a onclick=...>", so anchoring only on
// whitespace (the pre-fix behaviour) let a slash-separated handler ride out on an
// outbound signature / auto-reply body.
func TestSanitizerNeutralisesEventHandlerSeparators(t *testing.T) {
	live := []string{
		`<a onclick="alert(1)">y</a>`,   // whitespace separator (already covered)
		`<a/onclick="alert(1)">y</a>`,   // slash separator — the confirmed bypass
		"<a\tonclick=\"alert(1)\">y</a>", // tab separator
		`<img/src=x/onerror=alert(1)>`,  // slash-separated onerror
		`<xss/onmouseover=alert(1)>t`,   // unknown element + slash handler
	}
	for _, in := range live {
		out := strings.ToLower(sanitizeSnippetHTML(in))
		if strings.Contains(out, "onclick") || strings.Contains(out, "onerror") || strings.Contains(out, "onmouseover") {
			t.Errorf("event handler survived sanitisation:\n  in : %q\n  out: %q", in, out)
		}
	}
	// Guard against over-stripping: ordinary text/paths that merely contain "/on"
	// but no "on<word>=" attribute must be preserved verbatim.
	for _, keep := range []string{
		`Come /online now`,
		`<a href="/onboarding">welcome</a>`,
		`<p>ratio 1/on2</p>`,
	} {
		if got := sanitizeSnippetHTML(keep); got != keep {
			t.Errorf("false-positive strip:\n  in : %q\n  out: %q", keep, got)
		}
	}
}

// TestSanitizerNeutralisesObfuscatedSchemes pins that a javascript:/vbscript:
// scheme is neutralised even when hidden behind numeric HTML entities (a browser
// decodes these inside an href BEFORE resolving the scheme) or split with the
// tab/newline chars a browser strips from a URL.
func TestSanitizerNeutralisesObfuscatedSchemes(t *testing.T) {
	live := []string{
		`<a href="javascript:alert(1)">y</a>`,
		`<a href="&#106;avascript:alert(1)">y</a>`,     // leading j as decimal entity
		`<a href="java&#115;cript:alert(1)">y</a>`,     // interior s as decimal entity
		`<a href="&#x6a;avascript:alert(1)">y</a>`,     // hex entity
		`<a href="&#0000106;avascript:alert(1)">y</a>`, // zero-padded decimal
		"<a href=\"jav\tascript:alert(1)\">y</a>",      // literal tab split
		"<a href=\"jav&#x0A;ascript:alert(1)\">y</a>",  // newline entity split
		`<a href="vbscript:msgbox(1)">y</a>`,
	}
	for _, in := range live {
		out := sanitizeSnippetHTML(in)
		// A browser strips \t\n\r before scheme resolution, so collapse them here
		// before checking whether the scheme reconstitutes.
		flat := strings.ToLower(strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(out))
		if strings.Contains(flat, "javascript:") || strings.Contains(flat, "vbscript:") {
			t.Errorf("obfuscated scheme reconstituted:\n  in : %q\n  out: %q", in, out)
		}
	}
	// A decoded numeric entity must NOT be able to re-introduce live markup: only
	// scheme-relevant chars (letters/":"/whitespace) are decoded, never "<"/">".
	if o := sanitizeSnippetHTML(`&#60;script&#62;alert(1)&#60;/script&#62;`); strings.Contains(strings.ToLower(o), "<script") {
		t.Errorf("numeric entity re-introduced <script> markup: %q", o)
	}
	// Prose containing "Java script:" with a real SPACE (which a browser does NOT
	// strip) must survive — the scheme matcher tolerates only stripped controls.
	if in := "Learn Java script: a guide"; sanitizeSnippetHTML(in) != in {
		t.Errorf("false-positive on prose: %q -> %q", in, sanitizeSnippetHTML(in))
	}
}

// TestVacationBodySanitizedIntoOutboundReply is the end-to-end contract: a hostile
// vacation body stored via PUT /v1/settings/vacation is sanitised at rest, and the
// exact bytes that would be composed into the OUTBOUND auto-reply carry no active
// content — even for the slash-separator / entity-obfuscation bypasses.
func TestVacationBodySanitizedIntoOutboundReply(t *testing.T) {
	app, h := newParityApp(t)
	const owner = "ooo@gmail.com"

	hostileBody := `<p>Away.</p>` +
		`<a/onclick="steal()">x</a>` +
		`<img/src=q/onerror=steal()>` +
		`<a href="&#106;avascript:steal()">y</a>` +
		"<a href=\"jav\tascript:steal()\">z</a>" +
		`<script>steal()</script>`

	body, _ := json.Marshal(map[string]any{
		"enabled": true,
		"subject": "Out of office",
		"body":    hostileBody,
	})
	code, resp := doAs(t, app, owner, "PUT", "/v1/settings/vacation", string(body))
	if code != fiber.StatusOK {
		t.Fatalf("put vacation: %d %s", code, resp)
	}

	// Read the STORED body — this is precisely what the delivery path composes into
	// the outbound MIME text/html part.
	st := newSettingsStore(h.kv)
	var cfg vacationConfig
	if err := st.get(owner, kindVacation, &cfg); err != nil {
		t.Fatalf("load stored vacation: %v", err)
	}
	stored := strings.ToLower(strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(cfg.Body))
	for _, bad := range []string{"onclick", "onerror", "javascript:", "<script"} {
		if strings.Contains(stored, bad) {
			t.Fatalf("outbound vacation body still carries %q: %q", bad, cfg.Body)
		}
	}
	// Sanity: benign content survives so the responder is still useful.
	if !strings.Contains(cfg.Body, "Away.") {
		t.Fatalf("sanitiser dropped benign body content: %q", cfg.Body)
	}
}

// --- Per-owner isolation: the "|" key-delimiter hole --------------------------

// TestConnectedAccountsPipeOwnerCannotEnumerateNeighbour proves that an owner
// whose identity contains the "|" key delimiter cannot use the list prefix scan to
// read a DIFFERENT owner's connected accounts. Owner "a" must never see the
// records of owner "a|b" (whose keys are "a|b|<email>" and therefore share the
// "a|" list prefix). The store must fail closed on a "|"-bearing owner.
func TestConnectedAccountsPipeOwnerCannotEnumerateNeighbour(t *testing.T) {
	app, h := newParityApp(t)
	st := newAccountsStore(h.kv)

	// Victim owner literally named "a|b" (as could arrive from a mis-behaving
	// broker header): seed a connected account for them directly in the store.
	// save() itself now rejects a "|" owner, so seed at the KV layer to simulate a
	// record that a NON-fail-closed predecessor could have written.
	victimKey := connAccountKey("a|b", "secret@corp.com")
	rec, _ := json.Marshal(connectedAccount{Email: "secret@corp.com", Label: "victim", EncryptedPassword: encPw(t, "victimpw")})
	if err := h.kv.Set(connAccountsNS, victimKey, rec); err != nil {
		t.Fatalf("seed victim: %v", err)
	}

	// Attacker owner "a" lists — must NOT see "a|b"'s record.
	got, err := st.list("a")
	if err != nil {
		t.Fatalf("list as 'a': %v", err)
	}
	for _, acc := range got {
		if acc.Email == "secret@corp.com" {
			t.Fatalf("CROSS-OWNER LEAK: owner 'a' enumerated owner 'a|b's account: %+v", acc)
		}
	}

	// And a "|"-bearing owner is refused fail-closed at every store method.
	if _, err := st.list("a|b"); err != errBadKeyComponent {
		t.Fatalf("list('a|b') should fail closed, got %v", err)
	}
	if _, err := st.get("a|b", "secret@corp.com"); err == nil {
		t.Fatalf("get('a|b', ...) should not resolve a record")
	}
	if err := st.save("a|b", connectedAccount{Email: "x@y.com"}); err != errBadKeyComponent {
		t.Fatalf("save('a|b', ...) should fail closed, got %v", err)
	}

	// The HTTP surface is unaffected for a normal owner (regression sanity).
	stubDial(t, &fakeMailClient{}, nil)
	code, _ := doAs(t, app, "normal@gmail.com", "POST", "/v1/accounts",
		`{"email":"work@corp.com","password":"pw","imapServer":"imap.corp.com"}`)
	if code != fiber.StatusCreated {
		t.Fatalf("normal add should still work: %d", code)
	}
}

// TestSettingsPipeOwnerFailsClosed proves the settings store also refuses a
// "|"-bearing owner so a crafted identity cannot alias another owner's vacation /
// signatures blob.
func TestSettingsPipeOwnerFailsClosed(t *testing.T) {
	_, h := newParityApp(t)
	st := newSettingsStore(h.kv)

	if err := st.put("a|b", kindVacation, &vacationConfig{Enabled: true, Subject: "x"}); err != errBadKeyComponent {
		t.Fatalf("put with '|' owner should fail closed, got %v", err)
	}
	var cfg vacationConfig
	if err := st.get("a|b", kindVacation, &cfg); err != errBadKeyComponent {
		t.Fatalf("get with '|' owner should fail closed, got %v", err)
	}
}

// TestConnectedAccountPipeEmailRejected proves an account email carrying "|"
// cannot be stored (it would make the composite key ambiguous and could shadow a
// neighbouring owner's namespace).
func TestConnectedAccountPipeEmailRejected(t *testing.T) {
	_, h := newParityApp(t)
	st := newAccountsStore(h.kv)
	if err := st.save("owner@x.com", connectedAccount{Email: "a|b@evil.com"}); err != errBadKeyComponent {
		t.Fatalf("save with '|' email should fail closed, got %v", err)
	}
}

// --- Send-as identities: no spoof ---------------------------------------------

// TestIdentitiesAreReadOnlyAndNeverSpoofSender documents the structural anti-spoof
// property: there is no write path for stored identities, and the identities
// listing always leads with the authenticated primary — a user cannot register or
// return an alias they do not own. (The send path independently uses fromEmail for
// From; see messages.go.)
func TestIdentitiesAreReadOnlyAndNeverSpoofSender(t *testing.T) {
	app, h := newParityApp(t)
	const owner = "me@gmail.com"

	// Even if a stored-identities blob is planted directly (no handler writes it),
	// the GET must present the primary as owner and never mark a foreign alias
	// primary.
	st := newSettingsStore(h.kv)
	planted, _ := json.Marshal([]identity{{Address: "ceo@victim.com", IsPrimary: true}})
	if err := h.kv.Set(settingsNS, settingsKey(owner, kindIdentities), planted); err != nil {
		t.Fatalf("plant identities: %v", err)
	}
	_ = st

	code, b := doAs(t, app, owner, "GET", "/v1/settings/identities", "")
	if code != fiber.StatusOK {
		t.Fatalf("get identities: %d %s", code, b)
	}
	var out struct {
		Identities []identity `json:"identities"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Identities) == 0 || out.Identities[0].Address != owner || !out.Identities[0].IsPrimary {
		t.Fatalf("primary must lead and be the authenticated owner: %s", b)
	}
	for _, id := range out.Identities {
		if id.Address != owner && id.IsPrimary {
			t.Fatalf("a non-owner alias was marked primary (spoof surface): %+v", id)
		}
	}
}

// --- Credentials never in any /v1 response ------------------------------------

// TestNoConnectedAccountResponseCarriesSecret sweeps every /v1 account response
// (add, list, unified) and asserts the plaintext password, its ciphertext, and the
// encrypted-field name never appear.
func TestNoConnectedAccountResponseCarriesSecret(t *testing.T) {
	app, h := newParityApp(t)
	stubDial(t, &fakeMailClient{}, nil)
	const owner = "user@gmail.com"
	const secret = "pl4intext-secret"

	_, addResp := doAs(t, app, owner, "POST", "/v1/accounts",
		`{"email":"work@corp.com","password":"`+secret+`","imapServer":"imap.corp.com"}`)

	// The ciphertext as actually stored, so we can assert it is absent from bodies.
	acct, err := newAccountsStore(h.kv).get(owner, "work@corp.com")
	if err != nil {
		t.Fatalf("get stored: %v", err)
	}
	cipher := acct.EncryptedPassword

	_, listResp := doAs(t, app, owner, "GET", "/v1/accounts", "")
	// Broker path is stubbed so unified works too; a hostile limit is clamped.
	withStubbedDial(t, &fakeMailClient{}, nil)
	_, uniResp := doAs(t, app, owner, "GET", "/v1/unified?limit=4000000000", "")

	for _, body := range [][]byte{addResp, listResp, uniResp} {
		s := string(body)
		if strings.Contains(s, secret) {
			t.Fatalf("response leaked PLAINTEXT credential: %s", s)
		}
		if cipher != "" && strings.Contains(s, cipher) {
			t.Fatalf("response leaked ciphertext credential: %s", s)
		}
		if strings.Contains(strings.ToLower(s), "encryptedpassword") {
			t.Fatalf("response leaked the encrypted-password field: %s", s)
		}
	}
}

// TestUnifiedLimitClamped proves a hostile ?limit cannot be pushed into every
// connected account's fetch: the value handed to fetchAccountMessages is bounded
// by unifiedHardCap. We capture the limit the dial+fetch sees.
func TestUnifiedLimitClamped(t *testing.T) {
	app, h := newParityApp(t)
	const owner = "user@gmail.com"

	// Seed one connected account.
	if err := newAccountsStore(h.kv).save(owner, connectedAccount{
		Email: "work@corp.com", IMAPServer: "imap.corp.com", IMAPPort: 993,
		EncryptedPassword: encPw(t, "pw"),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Capture the limit passed to the connected-account FetchMessages.
	var sawLimit uint32
	orig := connectedAccountDial
	connectedAccountDial = func(string, int, string, string) (api.MailClient, error) {
		return &limitCapturingClient{onFetch: func(l uint32) { sawLimit = l }}, nil
	}
	t.Cleanup(func() { connectedAccountDial = orig })
	// Primary fetch goes through the broker dial seam.
	withStubbedDial(t, &fakeMailClient{}, nil)

	code, _ := doAs(t, app, owner, "GET", "/v1/unified?limit=4000000000", "")
	if code != fiber.StatusOK {
		t.Fatalf("unified: %d", code)
	}
	if sawLimit == 0 || sawLimit > unifiedHardCap {
		t.Fatalf("per-account fetch limit not clamped: saw %d (cap %d)", sawLimit, unifiedHardCap)
	}
}

// limitCapturingClient is a fakeMailClient that records the FetchMessages limit.
type limitCapturingClient struct {
	fakeMailClient
	onFetch func(uint32)
}

func (c *limitCapturingClient) FetchMessages(_ string, limit uint32) ([]models.Email, error) {
	if c.onFetch != nil {
		c.onFetch(limit)
	}
	return nil, nil
}
