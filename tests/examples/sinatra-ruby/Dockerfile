FROM ruby:3.3-alpine
RUN apk add --no-cache build-base
WORKDIR /app
COPY Gemfile ./
RUN bundle config set --local without 'development test' \
    && bundle install
COPY app.rb config.ru ./
EXPOSE 4567
CMD ["bundle", "exec", "rackup", "-o", "0.0.0.0", "-p", "4567"]
