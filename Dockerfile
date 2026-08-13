FROM golang:1.26 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/maidcafe-cloud ./cmd/cloud

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/maidcafe-cloud /maidcafe-cloud
COPY config.example.toml /config.example.toml

USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/maidcafe-cloud"]
CMD ["--config", "/etc/maidcafe/config.toml"]
