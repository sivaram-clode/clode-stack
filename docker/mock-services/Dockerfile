# --------------------------------------------------------------------------
# mock-services — small static binary in a distroless image. Talks to the
# host docker daemon over a bind-mounted /var/run/docker.sock, so it needs
# no privileges inside the container.
# --------------------------------------------------------------------------

FROM golang:1.25-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO off + static → distroless-static is enough at runtime.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/mock-services ./cmd/mock-services/

FROM alpine:3.19
RUN apk --no-cache add ca-certificates wget
COPY --from=builder /out/mock-services /mock-services
# 8080 matches the rest of the platform (brahmi, jumbo, raksha, ...).
# Map to any free host port at `docker run -p <host>:8080` time.
EXPOSE 8080
ENTRYPOINT ["/mock-services"]
CMD ["--addr", ":8080"]
