FROM denoland/deno:alpine-2.1.4
WORKDIR /app
COPY main.ts ./
# Pre-cache dependencies so the container starts offline-friendly.
RUN deno cache main.ts
ENV PORT=8000
EXPOSE 8000
CMD ["deno", "run", "--allow-net", "--allow-env", "main.ts"]
