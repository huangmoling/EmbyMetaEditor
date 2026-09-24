# syntax=docker/dockerfile:1

# ---------- 构建阶段 ----------
#
# 用 $BUILDPLATFORM 而不是默认的 $TARGETPLATFORM：Go 交叉编译本来就快，
# 在 buildx 多架构构建时不需要 QEMU 模拟，构建时间基本不变。
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# 先只拷依赖清单，让 go mod download 单独吃一层缓存 —— 改代码不会重下依赖
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0：产出纯静态二进制，容器里不需要 glibc / musl
# -trimpath / -buildvcs=false：与 Windows 那条构建命令保持一致，保证可复现
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -trimpath -buildvcs=false -ldflags "-s -w" -o /out/EmbyMetaEditor .

# ---------- 运行阶段 ----------
#
# 运行时要 ca-certificates（访问 Emby / MetaTube / javbus / gfriends 全是 HTTPS，
# 纯静态二进制在 Linux 上会去读 /etc/ssl/certs/ca-certificates.crt，
# 缺了这张表所有 HTTPS 请求都会证书校验失败）；
# tzdata 是为了让 TZ 生效、日期显示正确。
FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 1000 -s /sbin/nologin app \
 && mkdir -p /data \
 && chown -R app:app /data

COPY --from=build /out/EmbyMetaEditor /usr/local/bin/EmbyMetaEditor

# 数据目录：config.json（配置）+ cache/（gfriends 索引，约 10 MB）
# 用环境变量固定住，进程不管从哪里启动都会写到挂载卷里
#
# 访问认证：进程启动时会读 EMBYME_AUTH_USER / EMBYME_AUTH_PASSWORD
# （用 docker run -e 或 compose 的 environment 传进来）。都不传的话会生成一个
# 随机密码打到日志，`docker logs <容器>` 第一屏就能看到。
# 凭据只从运行时注入，不写进镜像 —— 镜像里任何东西都是公开可拉的。
ENV EMBYME_HOME=/data \
    TZ=Asia/Shanghai

WORKDIR /data
VOLUME ["/data"]
EXPOSE 8097

# 非 root 跑。绑定 1000 是因为大多数 NAS / Linux 宿主机的第一个普通用户就是 1000，
# 直接 -v 挂宿主目录时省得再 chown。
USER app

# 容器里必须监听 0.0.0.0，否则 -p 映射进来的流量到不了；
# -open=false 是因为容器里没有浏览器，别去调 xdg-open。
# 想换端口 / 加参数，在 docker run 后面追加即可，会覆盖 CMD。
ENTRYPOINT ["/usr/local/bin/EmbyMetaEditor"]
CMD ["-host", "0.0.0.0", "-port", "8097", "-open=false"]
