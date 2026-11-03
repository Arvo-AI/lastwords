FROM golang:1.26 AS build
WORKDIR /src
COPY main.go .
RUN go mod init consumer && CGO_ENABLED=0 go build -o /order-consumer .

FROM gcr.io/distroless/static-debian12
COPY --from=build /order-consumer /order-consumer
ENTRYPOINT ["/order-consumer"]
