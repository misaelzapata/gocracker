FROM composer:2 AS deps
WORKDIR /src
COPY composer.json ./
RUN composer install --no-dev --no-progress --no-interaction --optimize-autoloader --no-scripts

FROM php:8.3-cli-alpine
WORKDIR /app
COPY --from=deps /src/vendor ./vendor
COPY public ./public
EXPOSE 8080
CMD ["php", "-S", "0.0.0.0:8080", "-t", "public", "public/index.php"]
