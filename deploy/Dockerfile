# 有 Docker daemon 的环境可用本文件代替 `ko build`。基础镜像按 digest 钉死，升级 Go / AL2023 时同步改 digest。
FROM golang:1.26@sha256:9d2f36f06329b2a141b9db99ffa32765cf695ee57b813ca29e245e8670bcbfff AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM public.ecr.aws/amazonlinux/amazonlinux:2023-minimal@sha256:279612ae66723f3f83ffdfe0045c428827d167c1954b0883e018985b8bb1d3e3
COPY --from=build /out/gateway /usr/local/bin/gateway
USER 65532:65532
# 镜像里没有 curl，用网关自带的 -healthcheck 探 /healthz（K8s 不用这条，用清单里的 probe）
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/usr/local/bin/gateway", "-healthcheck", "-config", "/etc/llm-gateway/gateway.yaml"]
ENTRYPOINT ["/usr/local/bin/gateway"]
CMD ["-config", "/etc/llm-gateway/gateway.yaml"]
