FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# modernc.org/sqlite is pure Go, so the binary is static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/feishu-livechat ./cmd/feishu-livechat \
    && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/feishu-livechat /usr/local/bin/feishu-livechat
COPY --from=build --chown=65532:65532 /out/data /data
ENV LIVECHAT_DATA_DIR=/data LISTEN_ADDR=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["feishu-livechat"]
CMD ["serve"]
