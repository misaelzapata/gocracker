FROM alpine:latest
RUN apk add --no-cache busybox-extras
WORKDIR /var/www
COPY index.html .
EXPOSE 80
CMD ["httpd", "-f", "-p", "80", "-h", "/var/www"]
