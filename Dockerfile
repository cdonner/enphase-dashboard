# 1. UI: platform-independent output, so build it natively on the CI runner.
FROM --platform=$BUILDPLATFORM node:22-alpine AS ui
WORKDIR /app
COPY package*.json ./
RUN npm ci
COPY index.html vite.config.js ./
COPY src ./src
RUN npx vite build

# 2. Server: cross-compile a static Go binary with the UI embedded.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS server
ARG TARGETOS TARGETARCH TARGETVARIANT
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY --from=ui /app/dist ./dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w" -o /out/enphase-dashboard .

# 3. Nothing but the binary: no libc or shell, so it also runs on kernels that
# can't load ordinary userlands (e.g. 32-bit ARM NAS models with 32K pages).
FROM scratch
COPY --from=server /out/enphase-dashboard /enphase-dashboard
ENV HOST=0.0.0.0 PORT=3001 DATA_DIR=/app/data
VOLUME /app/data
EXPOSE 3001
ENTRYPOINT ["/enphase-dashboard"]
