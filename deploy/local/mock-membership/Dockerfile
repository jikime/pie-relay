# syntax=docker/dockerfile:1.7

FROM golang:1.25.12-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mock-membership .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/mock-membership /usr/local/bin/mock-membership
USER nonroot:nonroot
EXPOSE 18080
ENTRYPOINT ["/usr/local/bin/mock-membership"]

