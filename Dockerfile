# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.26.4 AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# Std-lib only -> a fully static binary for distroless.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/anon-ohttp-relay .

# ---- runtime ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/anon-ohttp-relay /anon-ohttp-relay
# Default RELAY_LISTEN port (override via env).
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/anon-ohttp-relay"]
