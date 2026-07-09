package jsonapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// mockTeamStore records the address/member it was built with (isolation asserts)
// and returns canned data, standing in for the brokered HTTP client to vulos-mail.
type mockTeamStore struct {
	address string
	member  string

	role      string
	members   []TeamMember
	convos    []TeamConvo
	convo     TeamConvo
	canned    []TeamCanned
	saved     TeamCanned
	present   []TeamPresence
	err       error
	lastNote  string
	noteCalls int
}

func (m *mockTeamStore) Access(context.Context) (string, error) { return m.role, m.err }
func (m *mockTeamStore) ListMembers(context.Context) ([]TeamMember, error) {
	return m.members, m.err
}
func (m *mockTeamStore) Members(context.Context, string, string, bool) ([]TeamMember, error) {
	return m.members, m.err
}
func (m *mockTeamStore) Convos(context.Context, []string, string, string) ([]TeamConvo, error) {
	return m.convos, m.err
}
func (m *mockTeamStore) Assign(_ context.Context, threadID, assignee string) (TeamConvo, error) {
	m.convo = TeamConvo{ThreadID: threadID, Assignee: assignee, Status: "open"}
	return m.convo, m.err
}
func (m *mockTeamStore) SetStatus(_ context.Context, threadID, status string) (TeamConvo, error) {
	m.convo = TeamConvo{ThreadID: threadID, Status: status}
	return m.convo, m.err
}
func (m *mockTeamStore) Seen(_ context.Context, threadID string) (TeamConvo, error) {
	return TeamConvo{ThreadID: threadID}, m.err
}
func (m *mockTeamStore) AddNote(_ context.Context, threadID, body string, mentions []string) (TeamConvo, error) {
	m.noteCalls++
	m.lastNote = body
	return TeamConvo{ThreadID: threadID, Status: "open", Notes: []TeamNote{{ID: "n1", Body: body, Mentions: mentions}}}, m.err
}
func (m *mockTeamStore) ListCanned(context.Context) ([]TeamCanned, error) { return m.canned, m.err }
func (m *mockTeamStore) SaveCanned(context.Context, string, string, string, bool) ([]TeamCanned, TeamCanned, error) {
	return m.canned, m.saved, m.err
}
func (m *mockTeamStore) Presence(context.Context, string, string, bool) ([]TeamPresence, error) {
	return m.present, m.err
}

// teamApp wires a brokered /v1 app whose team store is the given mock, capturing
// the address/member newTeamStore was built with so isolation can be asserted.
func teamApp(t *testing.T, mock *mockTeamStore) *fiber.App {
	t.Helper()
	t.Setenv(brokerEnvSecret, "s3cr3t")
	orig := newTeamStore
	newTeamStore = func(baseURL, secret, address, member string) teamStore {
		mock.address = address
		mock.member = member
		return mock
	}
	t.Cleanup(func() { newTeamStore = orig })
	h := newBrokerHandler(t)
	app := fiber.New()
	h.Register(app)
	return app
}

// teamReq builds a brokered request with the team-inbox URL + acting member set.
func teamReq(method, target, body string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		r.Header.Set(k, v)
	}
	// spec.Email = user@gmail.com is the SHARED mailbox here; the acting member is a
	// different account, both from the (secret-gated) broker headers.
	r.Header.Set(hdrMailTeamInboxURL, "http://mail.internal/internal/teaminbox")
	r.Header.Set(hdrMailMember, "agent@acme.test")
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

// The address handed to the store is ALWAYS the brokered mailbox, and the member is
// ALWAYS the brokered member — never client input.
func TestTeam_AddressAndMemberFromBrokerNotClient(t *testing.T) {
	mock := &mockTeamStore{role: "agent"}
	app := teamApp(t, mock)
	// Attacker tries to override address/member via query + body.
	req := teamReq("GET", "/v1/team/access?address=victim@evil.example&member=root@evil.example", "")
	resp, _ := app.Test(req)
	if resp.StatusCode != 200 {
		t.Fatalf("access: %d", resp.StatusCode)
	}
	if mock.address != "user@gmail.com" {
		t.Fatalf("address overridden by client: got %q want brokered mailbox", mock.address)
	}
	if mock.member != "agent@acme.test" {
		t.Fatalf("member overridden by client: got %q want brokered member", mock.member)
	}
}

// Without a team-inbox URL, the whole surface reports 501 (a personal account).
func TestTeam_DegradeNoTeamURL(t *testing.T) {
	mock := &mockTeamStore{}
	app := teamApp(t, mock)
	req := httptest.NewRequest("GET", "/v1/team/access", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	// deliberately NOT setting the team-inbox URL nor member
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusNotImplemented {
		t.Fatalf("no team URL: want 501, got %d", resp.StatusCode)
	}
}

// A team-inbox URL but no acting member → 400 (every op needs an actor).
func TestTeam_NoMember400(t *testing.T) {
	mock := &mockTeamStore{}
	app := teamApp(t, mock)
	req := httptest.NewRequest("GET", "/v1/team/access", nil)
	req.Header.Set(hdrBrokerAuth, "s3cr3t")
	for k, v := range brokeredHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set(hdrMailTeamInboxURL, "http://mail.internal/internal/teaminbox")
	// no member header
	resp, _ := app.Test(req)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("no member: want 400, got %d", resp.StatusCode)
	}
}

// An upstream 403 (member scoping) surfaces to the client as 403.
func TestTeam_UpstreamForbiddenSurfaces403(t *testing.T) {
	mock := &mockTeamStore{err: errTeamForbidden}
	app := teamApp(t, mock)
	resp, _ := app.Test(teamReq("GET", "/v1/team/members", ""))
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("forbidden upstream: want 403, got %d", resp.StatusCode)
	}
}

// Assign returns the convo; the thread id comes from the body.
func TestTeam_Assign(t *testing.T) {
	mock := &mockTeamStore{}
	app := teamApp(t, mock)
	resp, _ := app.Test(teamReq("POST", "/v1/team/assign", `{"threadId":"T1","assignee":"agent@acme.test"}`))
	if resp.StatusCode != 200 {
		t.Fatalf("assign: %d", resp.StatusCode)
	}
	var out struct {
		Convo TeamConvo `json:"convo"`
	}
	decode(t, resp.Body, &out)
	if out.Convo.ThreadID != "T1" || out.Convo.Assignee != "agent@acme.test" {
		t.Fatalf("assign convo: %+v", out.Convo)
	}
}

// A note is brokered to the team store's AddNote (a DISTINCT route from send). The
// send route (POST /v1/messages) is not this handler — a note has no send path.
func TestTeam_NoteRouteIsSeparateFromSend(t *testing.T) {
	mock := &mockTeamStore{}
	app := teamApp(t, mock)
	const note = "INTERNAL: do not tell the customer this"
	resp, _ := app.Test(teamReq("POST", "/v1/team/note", `{"threadId":"T1","body":"`+note+`","mentions":["admin@acme.test"]}`))
	if resp.StatusCode != 200 {
		t.Fatalf("note: %d", resp.StatusCode)
	}
	if mock.noteCalls != 1 || mock.lastNote != note {
		t.Fatalf("note not brokered to team store: calls=%d last=%q", mock.noteCalls, mock.lastNote)
	}
	var out struct {
		Convo TeamConvo `json:"convo"`
	}
	decode(t, resp.Body, &out)
	if len(out.Convo.Notes) != 1 || out.Convo.Notes[0].Body != note {
		t.Fatalf("note not returned: %+v", out.Convo)
	}
}

// Presence heartbeat returns the other agents present.
func TestTeam_Presence(t *testing.T) {
	mock := &mockTeamStore{present: []TeamPresence{{Account: "alice@acme.test", Activity: "replying"}}}
	app := teamApp(t, mock)
	resp, _ := app.Test(teamReq("POST", "/v1/team/presence", `{"threadId":"T1","activity":"viewing"}`))
	if resp.StatusCode != 200 {
		t.Fatalf("presence: %d", resp.StatusCode)
	}
	var out struct {
		Present []TeamPresence `json:"present"`
	}
	decode(t, resp.Body, &out)
	if len(out.Present) != 1 || out.Present[0].Account != "alice@acme.test" || out.Present[0].Activity != "replying" {
		t.Fatalf("presence: %+v", out.Present)
	}
}

// Canned list surfaces the team-shared saved replies.
func TestTeam_CannedList(t *testing.T) {
	mock := &mockTeamStore{canned: []TeamCanned{{ID: "c1", Title: "Greeting", Body: "Hello!"}}}
	app := teamApp(t, mock)
	resp, _ := app.Test(teamReq("GET", "/v1/team/canned", ""))
	if resp.StatusCode != 200 {
		t.Fatalf("canned: %d", resp.StatusCode)
	}
	var out struct {
		Canned []TeamCanned `json:"canned"`
	}
	decode(t, resp.Body, &out)
	if len(out.Canned) != 1 || out.Canned[0].Title != "Greeting" {
		t.Fatalf("canned: %+v", out.Canned)
	}
}
