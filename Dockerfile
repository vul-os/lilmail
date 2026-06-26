# lilmail — single-binary IMAP/SMTP webmail client (HTMX UI + /v1 JSON API).
#
# Build: docker build -t vulos/lilmail .
# Run:   docker run -p 3000:3000 -v $PWD/config.toml:/app/config.toml vulos/lilmail
#
# A config.toml must be present at /app/config.toml (mount it, or bake your own
# layer). Pure-Go build (CGO disabled) — bbolt and the optional pgx Postgres
# driver are both pure Go, so the result is a static binary on a minimal image.

# ── Stage 1: build the static binary ──────────────────────────────────────────
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /lilmail .

# ── Stage 2: minimal runtime ──────────────────────────────────────────────────
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /lilmail /app/lilmail
# Cache dir for the default embedded bbolt store (override with [storage] for Postgres).
RUN mkdir -p /app/cache
EXPOSE 3000
ENTRYPOINT ["/app/lilmail"]
