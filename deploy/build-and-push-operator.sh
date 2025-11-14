#!/bin/bash

set -e

TAG=develop-$(date +%Y%m%d%H%M%S)
REPOSITORY=${REPOSITORY:-openkruise/agent-sandbox-operator}
IMAGE=${REPOSITORY}:${TAG}

echo "Building image: ${IMAGE}"
docker build --platform linux/amd64 -t "${IMAGE}" -f dockerfiles/agent-sandbox-operator.Dockerfile .

echo "Pushing image: ${IMAGE}"
docker push "${IMAGE}"

echo "Removing image: ${IMAGE}"
docker rmi "${IMAGE}"

echo "Updating tag in deploy/helm/values.yaml to ${TAG}"
sed -i "" "/^sandboxOperator:/,/imagePullSecrets:/ {/tag:/ s/tag: .*/tag: ${TAG}/;}" deploy/helm/values.yaml
