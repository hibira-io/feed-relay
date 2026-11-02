FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/hibira-io/feed-relay/internal/relay.Version=${VERSION}" -o /feed-relay ./cmd/feed-relay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /feed-relay /feed-relay
WORKDIR /data
ENV CONFIG=/config/feed-relay.yaml STATE_FILE=/data/state.json
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/feed-relay"]
