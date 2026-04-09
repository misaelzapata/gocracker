FROM golang:1.22-alpine AS build
WORKDIR /src
COPY main.go .
RUN go build -o /app main.go

FROM scratch
COPY --from=build /app /app
EXPOSE 8080
ENTRYPOINT ["/app"]
