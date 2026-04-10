FROM rust:1.83-alpine AS build
RUN apk add --no-cache musl-dev
WORKDIR /src
COPY Cargo.toml ./
COPY src ./src
RUN cargo build --release && \
    cp target/release/actix-rust /actix-rust

FROM alpine:3.20
COPY --from=build /actix-rust /actix-rust
EXPOSE 8080
ENTRYPOINT ["/actix-rust"]
