FROM alpine:latest
RUN adduser -D -u 1000 app && mkdir -p /work && chown 1000:1000 /work
WORKDIR /work
USER 1000:1000
RUN id -u > build-user.txt
CMD ["sh", "-c", "id -u > runtime-user.txt && pwd > pwd.txt"]
