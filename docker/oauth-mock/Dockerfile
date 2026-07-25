# Mock OAuth provider endpoints (google, github) for local/CDP testing.
# Fiber app; not wired into gen-build-cache.sh's Go cache overlay, so it does a
# plain module download + build.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /oauth-mock .

FROM alpine:3.20
RUN apk add --no-cache wget
WORKDIR /app
COPY --from=build /oauth-mock /usr/local/bin/oauth-mock
COPY views ./views
EXPOSE 8080
ENTRYPOINT ["oauth-mock"]
