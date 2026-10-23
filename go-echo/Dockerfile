FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /go-echo .

FROM alpine:3.24
COPY --from=build /go-echo /usr/local/bin/go-echo
EXPOSE 8080
USER nobody
ENTRYPOINT ["/usr/local/bin/go-echo"]
