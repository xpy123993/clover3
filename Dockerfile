FROM golang as builder

WORKDIR /app

COPY go.mod ./
COPY go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w" -o clover3 .
RUN mkdir -p /etc/clover3
RUN mkdir -p /data
FROM gcr.io/distroless/base:nonroot

WORKDIR /bin
COPY --from=builder /data /data
COPY --from=builder /app/clover3 .
COPY --from=builder /etc/clover3 /etc/clover3

VOLUME /data
VOLUME /etc/clover3
USER nonroot:nonroot

ENTRYPOINT ["/bin/clover3"]