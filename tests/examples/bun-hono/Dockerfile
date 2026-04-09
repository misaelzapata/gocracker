FROM oven/bun:1.1-alpine
WORKDIR /app
COPY package.json ./
RUN bun install --no-save
COPY server.ts ./
ENV PORT=3000
EXPOSE 3000
CMD ["bun", "run", "server.ts"]
