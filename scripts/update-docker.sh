#!/bin/sh
# Update a Docker Compose / NAS installation of the Enphase dashboard to the latest image.
# The container is recreated from its own compose file, so its settings are kept.
#
#   sh update-docker.sh [container-name]      (default: solar-dashboard)
set -eu

NAME=${1:-solar-dashboard}

die() { echo "Error: $*" >&2; exit 1; }

command -v docker >/dev/null || die "docker not found (run this on the machine that runs the container)"
docker inspect "$NAME" >/dev/null 2>&1 || die "no container named '$NAME' (pass the name as the first argument)"

IMAGE=$(docker inspect -f '{{.Config.Image}}' "$NAME")
DIR=$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.project.working_dir" }}' "$NAME")
[ -n "$DIR" ] && [ -d "$DIR" ] || die "'$NAME' wasn't created from a compose file / NAS application; recreate it from compose.yaml"

if docker compose version >/dev/null 2>&1; then
  COMPOSE="docker compose"
elif command -v docker-compose >/dev/null; then
  COMPOSE="docker-compose"
else
  die "neither 'docker compose' nor 'docker-compose' is available"
fi

RUNNING=$(docker inspect -f '{{.Image}}' "$NAME")
echo "Pulling $IMAGE ..."
docker pull -q "$IMAGE" >/dev/null || die "pull failed"
LATEST=$(docker image inspect -f '{{.Id}}' "$IMAGE")

if [ "$RUNNING" = "$LATEST" ]; then
  echo "Already up to date."
  exit 0
fi

echo "New image found; recreating '$NAME' from $DIR ..."
cd "$DIR"
$COMPOSE up -d

VERSION=$(docker image inspect -f '{{ index .Config.Labels "org.opencontainers.image.version" }}' "$IMAGE" 2>/dev/null || true)
echo "Done. Running ${VERSION:-the latest image}."
echo "Old images can be removed with: docker image prune -f"
