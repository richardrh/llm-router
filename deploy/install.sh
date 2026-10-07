#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
NAMESPACE=${NAMESPACE:-llm-router}
IMAGE=${IMAGE:-ghcr.io/richardrh/llm-router:latest}
CONFIG=${CONFIG:-"$ROOT_DIR/router.yaml"}

command -v kubectl >/dev/null || { echo "kubectl is required" >&2; exit 1; }
[[ -f "$CONFIG" ]] || { echo "config file not found: $CONFIG" >&2; exit 1; }

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NAMESPACE" apply -f "$ROOT_DIR/deploy/kubernetes.yaml"

secret_args=(--from-literal=ROUTER_DEPLOYMENT=1)
[[ -n "${OPENROUTER_API_KEY:-}" ]] && secret_args+=(--from-literal="OPENROUTER_API_KEY=$OPENROUTER_API_KEY")
[[ -n "${ANTHROPIC_API_KEY:-}" ]] && secret_args+=(--from-literal="ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY")
kubectl -n "$NAMESPACE" create secret generic llm-router-keys \
  "${secret_args[@]}" --dry-run=client -o yaml | kubectl apply -f -

kubectl -n "$NAMESPACE" create configmap llm-router-config \
  --from-file=router.yaml="$CONFIG" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NAMESPACE" set image deployment/llm-router llm-router="$IMAGE"
kubectl -n "$NAMESPACE" rollout restart deployment/llm-router
kubectl -n "$NAMESPACE" rollout status deployment/llm-router --timeout=180s

printf 'llm-router is running in namespace %s with image %s\n' "$NAMESPACE" "$IMAGE"
