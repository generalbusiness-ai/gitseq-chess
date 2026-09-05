# One immutable image pins Go, Git, OpenSSH and their runtime libraries.
# Keeping the same toolchain image avoids an unpinned package-install layer.
FROM golang:1.26.7-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514
ARG CHESS_SOURCE=development
LABEL org.opencontainers.image.source="https://github.com/generalbusiness-ai/gitseq-chess"
LABEL org.opencontainers.image.revision="${CHESS_SOURCE}"
WORKDIR /build/chess
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -o /usr/local/bin/chess ./cmd/chess
WORKDIR /data
ENTRYPOINT ["/usr/local/bin/chess"]
CMD ["serve", "--repo", "/data", "--listen", "127.0.0.1:8080"]
