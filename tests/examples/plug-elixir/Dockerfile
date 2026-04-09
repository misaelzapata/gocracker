FROM elixir:1.17-alpine AS build
RUN mix local.hex --force && mix local.rebar --force
WORKDIR /src
COPY mix.exs ./
ENV MIX_ENV=prod
RUN mix deps.get && mix deps.compile
COPY lib ./lib
RUN mix compile && mix release --overwrite

FROM alpine:3.20
RUN apk add --no-cache libstdc++ openssl ncurses-libs
WORKDIR /app
COPY --from=build /src/_build/prod/rel/plug_app ./
ENV PORT=4000
EXPOSE 4000
CMD ["/app/bin/plug_app", "start"]
