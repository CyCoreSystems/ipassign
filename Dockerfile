FROM golang:alpine AS builder
ENV GO111MODULE on
RUN apk add --no-cache git
WORKDIR $GOPATH/src/github.com/CyCoreSystems/ipassign
COPY . .
RUN go get -d -v
RUN go build -o /go/bin/svc

FROM alpine
RUN apk add --no-cache ca-certificates
COPY --from=builder /go/bin/svc /go/bin/svc

ENTRYPOINT ["/go/bin/svc"]
