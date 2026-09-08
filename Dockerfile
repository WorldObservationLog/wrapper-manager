# ---- build stage ----
FROM golang:1.25 AS builder

WORKDIR /app

COPY . .
# For users in China, uncomment the line below to use a Go module proxy:
# RUN go env -w GO111MODULE=on && go env -w GOPROXY=https://goproxy.cn,direct
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -o wrapper-manager

# ---- runtime stage ----
# wrapper-lite-rootless needs user namespaces (unshare) and chroot, so the
# container must run with --privileged or with a seccomp/apparmor profile that
# allows them; Ubuntu (Debian-family) is a good base for this.
FROM ubuntu:24.04

WORKDIR /root/

# ca-certificates is required for HTTPS calls (nightly.link download, Apple).
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /app/wrapper-manager ./
RUN chmod +x ./wrapper-manager

# Persistent state: wrapper-lite payload + per-account instances + registry.
VOLUME ["/root/data"]

EXPOSE 8080

ENTRYPOINT ["./wrapper-manager"]
