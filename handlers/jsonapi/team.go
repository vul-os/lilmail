// handlers/jsonapi/team.go — /v1/team: collaborative shared / team inbox.
//
// SEAM: lilmail does not own team-inbox state. vulos-mail hosts the shared-mailbox
// model (membership + per-conversation assignment/status/notes + canned replies +
// collision presence) and exposes it via broker-gated /internal/teaminbox/*
// endpoints. In a CP-brokered deployment vulos-mail injects the team-inbox base URL
// as X-Vulos-Mail-Teaminbox-Url (only for a SHARED mailbox) and the acting member
// as X-Vulos-Mail-Member. These handlers broker every team op to that URL.
//
// ISOLATION / IDOR-SAFETY (load-bearing):
//   - `address` (the shared mailbox) is ALWAYS spec.Email — the validated brokered
//     mailbox, never client input. A caller can only operate on the mailbox the CP
//     authenticated them onto.
//   - `member` (the acting agent) is ALWAYS spec.Member — from the secret-gated
//     broker headers, never client input. vulos-mail authorizes THIS member against
//     the mailbox's ACL; a non-member gets 403 and observes nothing.
//   - A thread id / assignee / note in the request body is data the store validates
//     (assignee must be a member; unknown thread ids just yield a default record).
//
// NOTE-NEVER-LEAKS: the note route (POST /v1/team/note) is entirely separate from
// the send route (POST /v1/messages). A note is brokered to the team store and
// stored there; it is NEVER part of a compose/send payload. There is no code path
// from a team note into an outbound message.
//
// HONEST DEGRADE: when no team-inbox URL is brokered (a plain personal account, or
// standalone/session lilmail), the whole /v1/team surface reports 501 — the mailbox
// simply has no team-inbox layer.
package jsonapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// errTeamUnsupported is surfaced as 501 when the mailbox has no team-inbox layer
// (no X-Vulos-Mail-Teaminbox-Url brokered for this request).
var errTeamUnsupported = errors.New("team inbox is not available for this mailbox")

// errTeamNoMember is surfaced as 400 when the broker did not identify the acting
// member (X-Vulos-Mail-Member missing) — every team op needs an actor.
var errTeamNoMember = errors.New("no acting member for team inbox operation")

const maxTeamRespBytes = 4 << 20 // 4 MiB cap on any team-store response body

// Team DTOs mirror vulos-mail's teaminbox JSON shapes so the UI gets a stable
// contract regardless of backend.

type TeamMember struct {
	Account string `json:"account"`
	Role    string `json:"role"`
	AddedAt string `json:"addedAt,omitempty"`
}

type TeamNote struct {
	ID        string   `json:"id"`
	Author    string   `json:"author"`
	Body      string   `json:"body"`
	Mentions  []string `json:"mentions,omitempty"`
	CreatedAt string   `json:"createdAt"`
}

type TeamConvo struct {
	ThreadID  string     `json:"threadId"`
	Assignee  string     `json:"assignee,omitempty"`
	Status    string     `json:"status"`
	Notes     []TeamNote `json:"notes,omitempty"`
	SeenBy    []string   `json:"seenBy,omitempty"`
	UpdatedAt string     `json:"updatedAt,omitempty"`
}

type TeamCanned struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Author    string `json:"author,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type TeamPresence struct {
	Account  string `json:"account"`
	Activity string `json:"activity"`
	Since    string `json:"since,omitempty"`
}

// teamStore is the seam the team handlers use. httpTeamStore satisfies it in
// production; tests substitute a mock via newTeamStore.
type teamStore interface {
	Access(ctx context.Context) (string, error) // acting member's role, or error
	ListMembers(ctx context.Context) ([]TeamMember, error)
	Members(ctx context.Context, target, role string, remove bool) ([]TeamMember, error)
	Convos(ctx context.Context, threadIDs []string, status, assignedTo string) ([]TeamConvo, error)
	Assign(ctx context.Context, threadID, assignee string) (TeamConvo, error)
	SetStatus(ctx context.Context, threadID, status string) (TeamConvo, error)
	Seen(ctx context.Context, threadID string) (TeamConvo, error)
	AddNote(ctx context.Context, threadID, body string, mentions []string) (TeamConvo, error)
	ListCanned(ctx context.Context) ([]TeamCanned, error)
	SaveCanned(ctx context.Context, id, title, body string, del bool) ([]TeamCanned, TeamCanned, error)
	Presence(ctx context.Context, threadID, activity string, leave bool) ([]TeamPresence, error)
}

// newTeamStore builds the brokered HTTP team store client. Package var so tests
// can substitute a mock without a live vulos-mail.
var newTeamStore = func(baseURL, secret, address, member string) teamStore {
	return &httpTeamStore{
		base:    strings.TrimRight(baseURL, "/"),
		secret:  secret,
		address: address,
		member:  member,
		hc:      &http.Client{Timeout: 15 * time.Second},
	}
}

// teamStoreFor resolves the team store for a request. The team inbox requires the
// brokered path AND a team-inbox URL AND an acting member. Address is always the
// validated brokered mailbox; member is always the brokered member — neither is
// client input.
func (h *Handler) teamStoreFor(c *fiber.Ctx) (teamStore, error) {
	spec, ok := brokerSpecOf(c)
	if !ok || strings.TrimSpace(spec.TeamInboxURL) == "" {
		return nil, errTeamUnsupported
	}
	if strings.TrimSpace(spec.Member) == "" {
		return nil, errTeamNoMember
	}
	return newTeamStore(spec.TeamInboxURL, h.brokerSecret, spec.Email, spec.Member), nil
}

// registerTeam mounts the /v1/team routes. Called from Register only when the
// broker path is active; each handler still 501s if no team-inbox URL is brokered
// for the specific request (a personal account).
func (h *Handler) registerTeam(g fiber.Router) {
	t := g.Group("/team")
	t.Get("/access", h.handleTeamAccess)       // acting member's role (403 if not a member)
	t.Get("/members", h.handleTeamMembers)     // roster
	t.Post("/members", h.handleTeamMembersMut) // add/remove (admin only, enforced upstream)
	t.Post("/convos", h.handleTeamConvos)      // collaborative state for a page of threads
	t.Post("/assign", h.handleTeamAssign)      // assign/unassign a conversation
	t.Post("/status", h.handleTeamStatus)      // change status
	t.Post("/seen", h.handleTeamSeen)          // record seen-by/activity
	t.Post("/note", h.handleTeamNote)          // add internal note (NEVER outbound)
	t.Get("/canned", h.handleTeamCannedList)   // team-shared saved replies
	t.Post("/canned", h.handleTeamCannedMut)   // save/delete a saved reply
	t.Post("/presence", h.handleTeamPresence)  // collision heartbeat + who-else
}

func (h *Handler) handleTeamAccess(c *fiber.Ctx) error {
	store, err := h.teamStoreFor(c)
	if err != nil {
		return failTeam(c, err)
	}
	role, err := store.Access(c.Context())
	if err != nil {
		return failTeam(c, err)
	}
	return c.JSON(fiber.Map{"role": role})
}

func (h *Handler) handleTeamMembers(c *fiber.Ctx) error {
	store, err := h.teamStoreFor(c)
	if err != nil {
		return failTeam(c, err)
	}
	members, err := store.ListMembers(c.Context())
	if err != nil {
		return failTeam(c, err)
	}
	return c.JSON(fiber.Map{"members": nonNilMembers(members)})
}

func (h *Handler) handleTeamMembersMut(c *fiber.Ctx) error {
	store, err := h.teamStoreFor(c)
	if err != nil {
		return failTeam(c, err)
	}
	var req struct {
		Target string `json:"target"`
		Role   string `json:"role"`
		Remove bool   `json:"remove"`
	}
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad request")
	}
	members, err := store.Members(c.Context(), strings.TrimSpace(req.Target), strings.TrimSpace(req.Role), req.Remove)
	if err != nil {
		return failTeam(c, err)
	}
	return c.JSON(fiber.Map{"members": nonNilMembers(members)})
}

func (h *Handler) handleTeamConvos(c *fiber.Ctx) error {
	store, err := h.teamStoreFor(c)
	if err != nil {
		return failTeam(c, err)
	}
	var req struct {
		ThreadIDs  []string `json:"threadIds"`
		Status     string   `json:"status"`
		AssignedTo string   `json:"assignedTo"`
	}
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad request")
	}
	convos, err := store.Convos(c.Context(), req.ThreadIDs, strings.TrimSpace(req.Status), strings.TrimSpace(req.AssignedTo))
	if err != nil {
		return failTeam(c, err)
	}
	return c.JSON(fiber.Map{"convos": nonNilConvos(convos)})
}

func (h *Handler) handleTeamAssign(c *fiber.Ctx) error {
	return h.teamConvoMut(c, func(store teamStore, req teamConvoBody) (TeamConvo, error) {
		return store.Assign(c.Context(), req.ThreadID, req.Assignee)
	})
}

func (h *Handler) handleTeamStatus(c *fiber.Ctx) error {
	return h.teamConvoMut(c, func(store teamStore, req teamConvoBody) (TeamConvo, error) {
		return store.SetStatus(c.Context(), req.ThreadID, req.Status)
	})
}

func (h *Handler) handleTeamSeen(c *fiber.Ctx) error {
	return h.teamConvoMut(c, func(store teamStore, req teamConvoBody) (TeamConvo, error) {
		return store.Seen(c.Context(), req.ThreadID)
	})
}

func (h *Handler) handleTeamNote(c *fiber.Ctx) error {
	return h.teamConvoMut(c, func(store teamStore, req teamConvoBody) (TeamConvo, error) {
		return store.AddNote(c.Context(), req.ThreadID, req.Body, req.Mentions)
	})
}

// teamConvoBody is the shared request shape for per-conversation mutations.
type teamConvoBody struct {
	ThreadID string   `json:"threadId"`
	Assignee string   `json:"assignee"`
	Status   string   `json:"status"`
	Body     string   `json:"body"`
	Mentions []string `json:"mentions"`
}

func (h *Handler) teamConvoMut(c *fiber.Ctx, op func(teamStore, teamConvoBody) (TeamConvo, error)) error {
	store, err := h.teamStoreFor(c)
	if err != nil {
		return failTeam(c, err)
	}
	var req teamConvoBody
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad request")
	}
	if strings.TrimSpace(req.ThreadID) == "" {
		return fail(c, fiber.StatusBadRequest, "threadId is required")
	}
	convo, err := op(store, req)
	if err != nil {
		return failTeam(c, err)
	}
	return c.JSON(fiber.Map{"convo": convo})
}

func (h *Handler) handleTeamCannedList(c *fiber.Ctx) error {
	store, err := h.teamStoreFor(c)
	if err != nil {
		return failTeam(c, err)
	}
	canned, err := store.ListCanned(c.Context())
	if err != nil {
		return failTeam(c, err)
	}
	return c.JSON(fiber.Map{"canned": nonNilCanned(canned)})
}

func (h *Handler) handleTeamCannedMut(c *fiber.Ctx) error {
	store, err := h.teamStoreFor(c)
	if err != nil {
		return failTeam(c, err)
	}
	var req struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Delete bool   `json:"delete"`
	}
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad request")
	}
	list, one, err := store.SaveCanned(c.Context(), strings.TrimSpace(req.ID), strings.TrimSpace(req.Title), req.Body, req.Delete)
	if err != nil {
		return failTeam(c, err)
	}
	if req.Delete {
		return c.JSON(fiber.Map{"canned": nonNilCanned(list)})
	}
	return c.JSON(fiber.Map{"saved": one})
}

func (h *Handler) handleTeamPresence(c *fiber.Ctx) error {
	store, err := h.teamStoreFor(c)
	if err != nil {
		return failTeam(c, err)
	}
	var req struct {
		ThreadID string `json:"threadId"`
		Activity string `json:"activity"`
		Leave    bool   `json:"leave"`
	}
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad request")
	}
	if strings.TrimSpace(req.ThreadID) == "" {
		return fail(c, fiber.StatusBadRequest, "threadId is required")
	}
	present, err := store.Presence(c.Context(), req.ThreadID, strings.TrimSpace(req.Activity), req.Leave)
	if err != nil {
		return failTeam(c, err)
	}
	return c.JSON(fiber.Map{"present": nonNilPresence(present)})
}

// failTeam maps a team-store error to an HTTP response. Unsupported → 501, missing
// member → 400, an upstream-forbidden (member-scoping) surfaces as 403, anything
// else → 502.
func failTeam(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, errTeamUnsupported):
		return fail(c, fiber.StatusNotImplemented, err.Error())
	case errors.Is(err, errTeamNoMember):
		return fail(c, fiber.StatusBadRequest, err.Error())
	case errors.Is(err, errTeamForbidden):
		return fail(c, fiber.StatusForbidden, "not a member of this shared mailbox")
	case errors.Is(err, errTeamBadInput):
		return fail(c, fiber.StatusBadRequest, "invalid input")
	default:
		return fail(c, fiber.StatusBadGateway, "team inbox store unavailable")
	}
}

func nonNilMembers(m []TeamMember) []TeamMember {
	if m == nil {
		return []TeamMember{}
	}
	return m
}
func nonNilConvos(c []TeamConvo) []TeamConvo {
	if c == nil {
		return []TeamConvo{}
	}
	return c
}
func nonNilCanned(c []TeamCanned) []TeamCanned {
	if c == nil {
		return []TeamCanned{}
	}
	return c
}
func nonNilPresence(p []TeamPresence) []TeamPresence {
	if p == nil {
		return []TeamPresence{}
	}
	return p
}

// --- httpTeamStore: brokered HTTP client to vulos-mail's team-inbox store ------

// Upstream-mapped errors so the handler can translate the store's 403 (member
// scoping) into a client 403 without leaking anything else.
var (
	errTeamForbidden = errors.New("team store forbidden")
	errTeamBadInput  = errors.New("team store bad input")
)

type httpTeamStore struct {
	base    string // e.g. http://127.0.0.1:2080/internal/teaminbox
	secret  string // shared broker secret → X-Vulos-Broker-Auth
	address string // shared mailbox (spec.Email) → `address` (never client input)
	member  string // acting member (spec.Member) → `member` (never client input)
	hc      *http.Client
}

func (s *httpTeamStore) Access(ctx context.Context) (string, error) {
	q := url.Values{}
	q.Set("address", s.address)
	q.Set("member", s.member)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/access?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(hdrBrokerAuth, s.secret)
	var out struct {
		Role string `json:"role"`
	}
	if err := s.doJSON(req, &out); err != nil {
		return "", err
	}
	return out.Role, nil
}

func (s *httpTeamStore) ListMembers(ctx context.Context) ([]TeamMember, error) {
	q := url.Values{}
	q.Set("address", s.address)
	q.Set("member", s.member)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/members?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(hdrBrokerAuth, s.secret)
	var out struct {
		Members []TeamMember `json:"members"`
	}
	if err := s.doJSON(req, &out); err != nil {
		return nil, err
	}
	return out.Members, nil
}

func (s *httpTeamStore) Members(ctx context.Context, target, role string, remove bool) ([]TeamMember, error) {
	body := map[string]any{"address": s.address, "member": s.member, "target": target, "role": role, "remove": remove}
	var out struct {
		Members []TeamMember `json:"members"`
	}
	if err := s.post(ctx, "/members", body, &out); err != nil {
		return nil, err
	}
	return out.Members, nil
}

func (s *httpTeamStore) Convos(ctx context.Context, threadIDs []string, status, assignedTo string) ([]TeamConvo, error) {
	body := map[string]any{"address": s.address, "member": s.member, "threadIds": threadIDs, "status": status, "assignedTo": assignedTo}
	var out struct {
		Convos []TeamConvo `json:"convos"`
	}
	if err := s.post(ctx, "/convos", body, &out); err != nil {
		return nil, err
	}
	return out.Convos, nil
}

func (s *httpTeamStore) Assign(ctx context.Context, threadID, assignee string) (TeamConvo, error) {
	return s.convoPost(ctx, "/assign", map[string]any{"threadId": threadID, "assignee": assignee})
}

func (s *httpTeamStore) SetStatus(ctx context.Context, threadID, status string) (TeamConvo, error) {
	return s.convoPost(ctx, "/status", map[string]any{"threadId": threadID, "status": status})
}

func (s *httpTeamStore) Seen(ctx context.Context, threadID string) (TeamConvo, error) {
	return s.convoPost(ctx, "/seen", map[string]any{"threadId": threadID})
}

func (s *httpTeamStore) AddNote(ctx context.Context, threadID, body string, mentions []string) (TeamConvo, error) {
	return s.convoPost(ctx, "/note", map[string]any{"threadId": threadID, "body": body, "mentions": mentions})
}

// convoPost posts a per-conversation mutation and decodes the {convo} response.
func (s *httpTeamStore) convoPost(ctx context.Context, path string, extra map[string]any) (TeamConvo, error) {
	body := map[string]any{"address": s.address, "member": s.member}
	for k, v := range extra {
		body[k] = v
	}
	var out struct {
		Convo TeamConvo `json:"convo"`
	}
	if err := s.post(ctx, path, body, &out); err != nil {
		return TeamConvo{}, err
	}
	return out.Convo, nil
}

func (s *httpTeamStore) ListCanned(ctx context.Context) ([]TeamCanned, error) {
	q := url.Values{}
	q.Set("address", s.address)
	q.Set("member", s.member)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/canned?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(hdrBrokerAuth, s.secret)
	var out struct {
		Canned []TeamCanned `json:"canned"`
	}
	if err := s.doJSON(req, &out); err != nil {
		return nil, err
	}
	return out.Canned, nil
}

func (s *httpTeamStore) SaveCanned(ctx context.Context, id, title, body string, del bool) ([]TeamCanned, TeamCanned, error) {
	reqBody := map[string]any{"address": s.address, "member": s.member, "id": id, "title": title, "body": body, "delete": del}
	var out struct {
		Canned []TeamCanned `json:"canned"`
		Saved  TeamCanned   `json:"canned_saved"`
		One    TeamCanned   `json:"saved"`
	}
	if err := s.post(ctx, "/canned", reqBody, &out); err != nil {
		return nil, TeamCanned{}, err
	}
	return out.Canned, out.One, nil
}

func (s *httpTeamStore) Presence(ctx context.Context, threadID, activity string, leave bool) ([]TeamPresence, error) {
	body := map[string]any{"address": s.address, "member": s.member, "threadId": threadID, "activity": activity, "leave": leave}
	var out struct {
		Present []TeamPresence `json:"present"`
	}
	if err := s.post(ctx, "/presence", body, &out); err != nil {
		return nil, err
	}
	return out.Present, nil
}

// post marshals body and performs a POST to base+path, decoding into out.
func (s *httpTeamStore) post(ctx context.Context, path string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set(hdrBrokerAuth, s.secret)
	req.Header.Set("Content-Type", "application/json")
	return s.doJSON(req, out)
}

// doJSON performs the request and decodes a JSON response into out. The body is
// read through a LimitReader so a hostile/broken upstream can't exhaust memory. A
// 403 upstream (member scoping) becomes errTeamForbidden; a 400 becomes
// errTeamBadInput; any other non-2xx or transport error becomes a generic store
// error (502).
func (s *httpTeamStore) doJSON(req *http.Request, out any) error {
	resp, err := s.hc.Do(req)
	if err != nil {
		return errors.New("team store unreachable")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxTeamRespBytes))
	if resp.StatusCode == http.StatusForbidden {
		return errTeamForbidden
	}
	if resp.StatusCode == http.StatusBadRequest {
		return errTeamBadInput
	}
	if resp.StatusCode == http.StatusNotImplemented {
		return errTeamUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("team store error")
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return errors.New("invalid team store response")
		}
	}
	return nil
}
