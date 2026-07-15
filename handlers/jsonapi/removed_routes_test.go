package jsonapi

// removed_routes_test.go — regression guard for the central-coupling removal.
//
// lilmail is a standalone client: it must NOT expose the /v1 surfaces that only
// ever reverse-proxied to a central vulos-mail engine (rules/threads/categories/
// smart-folders/team-inbox/spam-settings). Those were deleted; this test locks
// them out so a future change cannot silently reintroduce a central-coupled
// endpoint. An authenticated request to any of them must 404 (route not
// registered), never reach a handler.

import (
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestRemovedCentralCoupledRoutesAreGone(t *testing.T) {
	app := newBrokeredApp(t, &fakeMailClient{})

	cases := []struct{ method, path string }{
		{"GET", "/v1/rules"},
		{"POST", "/v1/rules"},
		{"PUT", "/v1/rules/r1"},
		{"DELETE", "/v1/rules/r1"},
		{"POST", "/v1/rules/reorder"},
		{"POST", "/v1/rules/run"},
		{"GET", "/v1/threads/abc"},
		{"POST", "/v1/messages/1/category"},
		{"POST", "/v1/messages/1/smartfolder"},
		{"GET", "/v1/team/messages"},
		{"POST", "/v1/team/note"},
		{"GET", "/v1/settings/spam"},
		{"PUT", "/v1/settings/spam"},
	}
	for _, tc := range cases {
		resp, err := app.Test(fReq(tc.method, tc.path, ""))
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		if resp.StatusCode != fiber.StatusNotFound {
			t.Fatalf("%s %s: got %d, want 404 (route must not exist in a standalone client)",
				tc.method, tc.path, resp.StatusCode)
		}
	}
}
