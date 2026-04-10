FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fiber-api .

FROM scratch
COPY --from=build /out/fiber-api /fiber-api
EXPOSE 8080
ENTRYPOINT ["/fiber-api"]
