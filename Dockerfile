ARG BASE=node:22-alpine
# The UI build output is platform-independent, so build it natively on the CI runner.
FROM --platform=$BUILDPLATFORM node:22-alpine AS build
WORKDIR /app
COPY package*.json ./
RUN npm ci
COPY index.html vite.config.js ./
COPY src ./src
RUN npm run build

# Runtime base: node:22-alpine (default) or node:22-slim (Debian), via --build-arg BASE=...
FROM ${BASE}
WORKDIR /app
ENV NODE_ENV=production HOST=0.0.0.0 PORT=3001
RUN if command -v apk >/dev/null; then apk add --no-cache tzdata; \
    else apt-get update && apt-get install -y --no-install-recommends tzdata && rm -rf /var/lib/apt/lists/*; fi
COPY package*.json ./
RUN npm ci --omit=dev && npm cache clean --force
COPY server ./server
COPY --from=build /app/dist ./dist
RUN mkdir -p data && chown node:node data
USER node
VOLUME /app/data
EXPOSE 3001
CMD ["node", "server/index.js"]
