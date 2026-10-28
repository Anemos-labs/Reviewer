# The reviewer and the sleipnir it runs, in one image. Reviews run their commands in Docker sandboxes started through the
# host's engine (see docker-compose.yml), so the image carries the Docker CLI, not an engine.
ARG GO_VERSION=1.26.10

FROM golang:${GO_VERSION}-bookworm AS build
ARG SLEIPNIR_REF=main
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/reviewer ./cmd/reviewer
RUN git clone --filter=blob:none https://github.com/anemos-labs/sleipnir /sleipnir \
 && cd /sleipnir && git checkout --detach "$SLEIPNIR_REF" \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sleipnir ./cmd/sleipnir

FROM docker:27.3.1-cli
RUN apk add --no-cache git bash ca-certificates tini
COPY --from=build /out/reviewer /out/sleipnir /usr/local/bin/
# the scan plugin is off by default in sleipnir: the reviewer's sleipnir has it on
RUN mkdir -p /root/.sleipnir && printf '{"plugins":{"enabled":{"scan":true}}}\n' > /root/.sleipnir/config.json
ENV REVIEWER_WORK=/srv/reviewer/work SLEIPNIR_BIN=/usr/local/bin/sleipnir
EXPOSE 8080
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/reviewer"]
