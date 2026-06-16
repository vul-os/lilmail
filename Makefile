.PHONY: build test vet screenshots demo-screenshots clean

# Build the lilmail binary
build:
	go build -o lilmail .

# Run all tests
test:
	go test ./...

# Vet
vet:
	go vet ./...

# Run build + vet + test
check: build vet test

# Regenerate docs/screenshots/*.png using Playwright.
# Boots lilmail with a minimal demo config (login page captured without credentials).
# Set LILMAIL_* env vars for inbox/message/compose/settings screenshots (see docs/SCREENSHOTS.md).
screenshots: build
	@echo "==> Installing Playwright dependencies (first run only)..."
	cd scripts && npm install --silent && npx --yes playwright install chromium 2>/dev/null || true
	@echo "==> Running screenshotter..."
	cd scripts && node screenshots.mjs
	@echo "==> Screenshots written to docs/screenshots/"

# Regenerate screenshots using the in-memory demo mode (no IMAP/SMTP required).
# Seeds the inbox with realistic test messages via [demo] enabled = true.
demo-screenshots: build
	@echo "==> Installing Playwright dependencies (first run only)..."
	cd scripts && npm install --silent && npx --yes playwright install chromium 2>/dev/null || true
	@echo "==> Starting demo server and capturing screenshots..."
	scripts/seed-demo.sh --screenshots
	@echo "==> Demo screenshots written to docs/screenshots/"

clean:
	rm -f lilmail
