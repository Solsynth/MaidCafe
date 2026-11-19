# syntax=docker/dockerfile:1

# ---- SPA build (internal/cloud/web) ----
FROM oven/bun:1.4-alpine AS web
WORKDIR /web
COPY internal/cloud/web/package.json internal/cloud/web/bun.lock ./
RUN bun install --frozen-lockfile
COPY internal/cloud/web/ ./
# Skip vue-tsc here: the build stage must not depend on a host-only TypeScript
# bridge, and the Go image has no Node toolchain to run it.
RUN bun run build-only

# ---- Go build ----
FROM golang:1.26 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/maidcafe-cloud ./cmd/cloud

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/maidcafe-cloud /app/maidcafe-cloud
# [web] distPath defaults to internal/cloud/web/dist relative to the working
# directory, so the console lands at the path the built binary looks for.
COPY --from=web /web/dist /app/internal/cloud/web/dist
COPY config.cloud.example.toml /app/config.cloud.example.toml

USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/maidcafe-cloud"]
CMD ["--config", "/etc/maidcafe/config.toml"]
