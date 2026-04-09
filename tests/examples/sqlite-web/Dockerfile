# sqlite-web: small Go HTTP service backed by a local SQLite file.
#
# Build with:    gocracker run --dockerfile tests/examples/sqlite-web/Dockerfile \
#                              --context  tests/examples/sqlite-web --kernel ...
# Verify with:   curl -X POST -d 'hello' :8080/notes ; curl :8080/notes
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sqlite-web .

FROM scratch
COPY --from=build /out/sqlite-web /sqlite-web
ENV PORT=8080 SQLITE_PATH=/data/notes.db
EXPOSE 8080
ENTRYPOINT ["/sqlite-web"]
