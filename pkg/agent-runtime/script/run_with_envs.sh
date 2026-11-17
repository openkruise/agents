#!/bin/bash
# run_with_envs.sh is the PID-1 wrapper for sandboxes that declare
# spec.startupMode=Deferred. It holds the main process until the
# claim-time env file and the readiness signal have been delivered, then
# sources the env file and exec's the original command with those variables
# exported. This is what makes claim-time envVars visible to the main process
# (a process environment cannot be changed after container start) and lets the
# main process start only after the sandbox is claimed.
#
# Usage:
#   run_with_envs.sh -- <cmd> [args...]          wait mode (main process)
#   run_with_envs.sh --probe -- <probe> [args]   probe mode (startup/liveness/readiness)
#   run_with_envs.sh --help
#
# Environment (injected by the controller):
#   SANDBOX_ENV_DIR  directory holding env.sh + .env_ready (default /mnt/envd/claim)
#   SANDBOX_POD_UID  pod UID (downward API); the readiness signal must carry the
#                    same value or it is ignored (generation check)
#
# File protocol (written by the claim-time delivery channel):
#   $SANDBOX_ENV_DIR/env.sh      `export KEY='value'` lines
#   $SANDBOX_ENV_DIR/cmd.sh      optional claim command (`exec 'arg0' ...`),
#                                sourced after env.sh in main-command mode
#   $SANDBOX_ENV_DIR/.env_ready  readiness signal, content = pod UID
#
# Probe mode semantics: before the signal arrives, probe commands succeed
# immediately so a pre-warmed (not yet claimed) pod passes its kubelet probes
# and stays healthy; after the signal arrives the real probe command runs with
# the claim env sourced. Non-exec probes cannot be wrapped by this script.

set -u

ENV_DIR="${SANDBOX_ENV_DIR:-/mnt/envd/claim}"
ENV_SCRIPT="$ENV_DIR/env.sh"
CMD_SCRIPT="$ENV_DIR/cmd.sh"
ENV_READY="$ENV_DIR/.env_ready"
POD_UID="${SANDBOX_POD_UID:-}"

PROBE=false
CMD=()

usage() {
    echo "Usage: run_with_envs.sh [--probe] -- <cmd> [args...]"
    echo "  --probe   probe mode: succeed immediately until the readiness signal,"
    echo "            then run the real command with the claim env sourced"
    echo "  --help    show this help message"
}

while [ $# -gt 0 ]; do
    case "$1" in
        --probe)
            PROBE=true
            shift
            ;;
        --)
            shift
            CMD=("$@")
            shift $#
            ;;
        --help|-h)
            usage
            exit 0
            ;;
        *)
            echo "[run-with-envs] Error: unknown argument '$1'" >&2
            usage >&2
            exit 1
            ;;
    esac
done

# An empty template command is valid for a Deferred pool: the claim supplies
# $SANDBOX_ENV_DIR/cmd.sh. The final check after the signal decides whether
# anything is runnable at all.

# signal_ready reports whether the readiness signal for THIS pod generation is
# present: the file must exist and — when the generation uid is injected — its
# content must match SANDBOX_POD_UID. A stale signal from another generation is
# treated as not ready.
signal_ready() {
    [ -f "$ENV_READY" ] || return 1
    if [ -n "$POD_UID" ]; then
        local content
        content=$(cat "$ENV_READY" 2>/dev/null)
        [ "$content" = "$POD_UID" ] || return 1
    fi
    return 0
}

# source_env loads the claim env file into the current shell. A broken env file
# aborts startup with a non-zero exit instead of exec'ing the command with a
# silently incomplete environment.
source_env() {
    if [ ! -f "$ENV_SCRIPT" ]; then
        return 0
    fi
    set -a
    # shellcheck disable=SC1090
    if ! . "$ENV_SCRIPT"; then
        echo "[run-with-envs] Error: failed to source '$ENV_SCRIPT'" >&2
        return 1
    fi
    set +a
    return 0
}

if [ "$PROBE" = "true" ]; then
    # Probe mode: keep the pre-warmed pod healthy before the claim. The real
    # probe command only runs once the env for the current generation is ready.
    if ! signal_ready; then
        exit 0
    fi
    source_env || exit 1
    exec "${CMD[@]}"
fi

# Wait mode (PID 1): block until the claim-time env for this generation is
# delivered. There is deliberately no timeout: an unclaimed pod stays in the
# warming pool and is cleaned up by the SandboxSet lifecycle, while a claimed
# pod is guaranteed a signal by the delivery channel (which fail-closes the
# claim when it cannot deliver).
echo "[run-with-envs] waiting for claim-time env (dir=$ENV_DIR, podUID=${POD_UID:-unset}) ..."
while ! signal_ready; do
    sleep 1
done

source_env || exit 1

# The claim command overrides the template command. cmd.sh is generated with
# `exec 'arg0' 'arg1' ...`, so sourcing it replaces this shell (and inherits the
# claim environment sourced above). Probe mode never consults it.
if [ -f "$CMD_SCRIPT" ]; then
    echo "[run-with-envs] env ready, executing claim command from $CMD_SCRIPT"
    # shellcheck disable=SC1090
    if ! . "$CMD_SCRIPT"; then
        echo "[run-with-envs] Error: failed to source '$CMD_SCRIPT'" >&2
        exit 1
    fi
fi

if [ ${#CMD[@]} -eq 0 ]; then
    echo "[run-with-envs] Error: no command to run: the claim does not set spec.command and the template declares none" >&2
    exit 1
fi
echo "[run-with-envs] env ready, starting: ${CMD[*]}"
exec "${CMD[@]}"
