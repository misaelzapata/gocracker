FROM alpine:latest
RUN apk add --no-cache busybox-extras
WORKDIR /srv/www
RUN printf 'compose-basic\n' > index.html
CMD ["httpd", "-f", "-p", "8080", "-h", "/srv/www"]
