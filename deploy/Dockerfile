# yanshi 的镜像：一个二进制，按 serve -roles 裁剪成不同的角色（docs/design/deployment.md）。
# 构建：docker build -f deploy/Dockerfile -t yanshi:<版本> .
# 网页通话页与调试控制台的静态文件随二进制嵌入（internal/webui），打包产物已在仓库中。
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/yanshi ./cmd/yanshi

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/yanshi /yanshi
USER nonroot:nonroot
ENTRYPOINT ["/yanshi"]
