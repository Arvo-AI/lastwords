FROM golang:1.26 AS build
WORKDIR /src
COPY main.go .
RUN go mod init billing && CGO_ENABLED=0 go build -o /billing-api .

FROM gcr.io/distroless/static-debian12
COPY --from=build /billing-api /billing-api
ENTRYPOINT ["/billing-api"]
