# 协同止付工作单服务镜像：同时构建服务与验收程序。
# 依赖已全部 vendor，构建无需访问网络。
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -mod=vendor -trimpath -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -mod=vendor -trimpath -o /out/acceptance ./cmd/acceptance

FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/server /usr/local/bin/server
COPY --from=build /out/acceptance /usr/local/bin/acceptance
COPY fixtures /app/fixtures
ENV FIXTURES_DIR=/app/fixtures \
    LISTEN_ADDR=:8080
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
