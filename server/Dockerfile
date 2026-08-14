# syntax=docker/dockerfile:1
#
# Pie Relay 서버 이미지 — Desktop ws(/ws/participant) ⇄ clientd ws(/ws/agent)
# 양 축 공유 시크릿(RELAY_JWT_SECRET)으로 scoped JWT를 오프라인 검증한다.
# 모바일 resume credential의 해시 상태만 선택적으로 영속화한다.
#  - Go 멀티스테이지: build → distroless static(nonroot) 최소 런타임.
#  - 리슨 포트는 --addr 로 주입 (기본 :13412).
# build: docker build -f Dockerfile -t pie-relay-server:latest .
FROM golang:1.25.12-alpine AS build
WORKDIR /src
COPY . /src/
RUN go mod download && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/relay ./cmd/relay && \
    mkdir -p /out/state

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/relay /usr/local/bin/relay
COPY --from=build --chown=65532:65532 /out/state /var/lib/pie-relay/relay
USER nonroot:nonroot
EXPOSE 13412
# distroless 에는 shell/curl 이 없어 healthcheck 는 바이너리 probe 모드를 재사용한다:
#   healthcheck: ["CMD", "/usr/local/bin/relay", "-healthcheck=http://127.0.0.1:13412/healthz"]
ENTRYPOINT ["/usr/local/bin/relay"]
