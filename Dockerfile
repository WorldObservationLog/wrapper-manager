FROM golang:1.23 as builder

WORKDIR /app

COPY . .
# RUN go env -w GO111MODULE=on && go env -w GOPROXY=https://goproxy.cn,direct
RUN go mod tidy
RUN GOOS=linux go build -o wrapper-manager

FROM ubuntu:latest

WORKDIR /root/

COPY --from=builder /app/wrapper-manager .
RUN apt-get update && apt-get install -y ca-certificates
RUN chmod +x ./wrapper-manager

ENTRYPOINT ["./wrapper-manager"]
EXPOSE 8080