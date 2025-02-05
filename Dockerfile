FROM golang:1.23.6 
WORKDIR /app
COPY . .
RUN go build  -o pwnd cmd/pwnd/main.go
RUN go build  -o pwngen cmd/pwngen/main.go
FROM alpine:latest
RUN apk --no-cache add ca-certificates libc6-compat
COPY --from=0 /app/pwnd /app/pwngen /app/
WORKDIR /data
VOLUME /data 
CMD ["/app/pwnd", "--database", "/data/pwned-passwords.bin"]