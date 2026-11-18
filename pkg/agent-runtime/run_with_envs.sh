#!/bin/bash
# run_with_envs.sh is the PID-1 wrapper for sandboxes that declare
# spec.startupMode=Deferred. It holds the main process until the
# claim-time env file and the readiness signal have been delivered, then
# sources the env file and exec's the claim or template command with those
# variables exported. This is what makes claim-time envVars visible to the main
# process (a process environment cannot be changed after container start) and
# lets the main process start only after the sandbox is claimed.
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
#   $SANDBOX_ENV_DIR/cmd.sh      optional claim command (`exec -- 'arg0' ...`),
#                                sourced after env.sh in main-command mode
#   $SANDBOX_ENV_DIR/.env_ready  readiness signal, content = pod UID
#
# Probe mode semantics: before the signal arrives, probe commands succeed
# immediately so a pre-warmed (not yet claimed) pod passes its kubelet probes
# and stays healthy; after the signal arrives the real probe command runs with
# the claim env sourced. Non-exec probes cannot be wrapped by this script.
#
# Internal variables use the _rwe_ prefix and the command stays in the
# positional parameters, so neither the template env nor the sourced claim env
# can clobber the wrapper state, and the app receives its variables unchanged.

set -u

_rwe_env_dir="${SANDBOX_ENV_DIR:-/mnt/envd/claim}"
_rwe_env_script="$_rwe_env_dir/env.sh"
_rwe_cmd_script="$_rwe_env_dir/cmd.sh"
_rwe_env_ready="$_rwe_env_dir/.env_ready"
_rwe_pod_uid="${SANDBOX_POD_UID:-}"
_rwe_probe=false

_rwe_usage() {
    echo "Usage: run_with_envs.sh [--probe] -- <cmd> [args...]"
    echo "  --probe   probe mode: succeed immediately until the readiness signal,"
    echo "            then run the real command with the claim env sourced"
    echo "  --help    show this help message"
}

while [ $# -gt 0 ]; do
    case "$1" in
        --probe)
            _rwe_probe=true
            shift
            ;;
        --)
            shift
            break
            ;;
        --help|-h)
            _rwe_usage
            exit 0
            ;;
        *)
            echo "[run-with-envs] Error: unknown argument '$1'" >&2
            _rwe_usage >&2
            exit 1
            ;;
    esac
done

# _rwe_signal_ready reports whether the readiness signal for THIS pod generation
# is present: the file must exist and — when the generation uid is injected —
# its content must match SANDBOX_POD_UID. A stale signal from another
# generation is treated as not ready.
_rwe_signal_ready() {
    [ -f "$_rwe_env_ready" ] || return 1
    if [ -n "$_rwe_pod_uid" ]; then
        [ "$(cat "$_rwe_env_ready" 2>/dev/null)" = "$_rwe_pod_uid" ] || return 1
    fi
    return 0
}

# _rwe_source_env loads the claim env file into the current shell. `.` only
# reports the status of the last command in the file, so a strict pre-check in
# a separate shell (set -e is ignored inside `if !`) first catches a failing
# line anywhere, e.g. an assignment to a read-only variable such as UID. A
# broken env file aborts startup instead of exec'ing the command with a
# silently incomplete environment.
_rwe_source_env() {
    [ -f "$_rwe_env_script" ] || return 0
    if ! "$BASH" -ec '. "$1"' _ "$_rwe_env_script" >/dev/null; then
        echo "[run-with-envs] Error: '$_rwe_env_script' does not apply cleanly" >&2
        return 1
    fi
    set -a
    # shellcheck disable=SC1090
    if ! . "$_rwe_env_script"; then
        echo "[run-with-envs] Error: failed to source '$_rwe_env_script'" >&2
        return 1
    fi
    set +a
    return 0
}

if [ "$_rwe_probe" = "true" ]; then
    if [ $# -eq 0 ]; then
        echo "[run-with-envs] Error: no probe command to run" >&2
        exit 1
    fi
    # Probe mode: keep the pre-warmed pod healthy before the claim. The real
    # probe command only runs once the env for the current generation is ready.
    _rwe_signal_ready || exit 0
    _rwe_source_env || exit 1
    exec -- "$@"
fi

# Wait mode (PID 1): block until the claim-time env for this generation is
# delivered. There is deliberately no timeout: an unclaimed pod stays in the
# warming pool and is cleaned up by the SandboxSet lifecycle, while a claimed
# pod is guaranteed a signal by the delivery channel (which fail-closes the
# claim when it cannot deliver).
#
# As PID 1, bash only receives signals it has a handler for, so the traps let
# an unclaimed pod stop promptly instead of waiting out its grace period.
# `sleep & wait` lets a trap fire without waiting for the current sleep, and
# exec resets the traps, so the app keeps default signal handling.
trap 'exit 143' TERM
trap 'exit 130' INT
echo "[run-with-envs] waiting for claim-time env (dir=$_rwe_env_dir, podUID=${_rwe_pod_uid:-unset}) ..."
while ! _rwe_signal_ready; do
    sleep 1 &
    wait $!
done

_rwe_source_env || exit 1

# The claim command overrides the template command. cmd.sh is generated as a
# single `exec -- 'arg0' 'arg1' ...` line, so sourcing it replaces this shell
# (and inherits the claim environment sourced above). Returning from it means
# the claim command never started; never fall back to the template command.
# Probe mode never consults it.
if [ -f "$_rwe_cmd_script" ]; then
    echo "[run-with-envs] env ready, executing claim command from $_rwe_cmd_script"
    # shellcheck disable=SC1090
    . "$_rwe_cmd_script"
    echo "[run-with-envs] Error: '$_rwe_cmd_script' returned without exec'ing the claim command" >&2
    exit 1
fi

if [ $# -eq 0 ]; then
    echo "[run-with-envs] Error: no command to run: the claim does not set spec.command and the template declares none" >&2
    exit 1
fi
echo "[run-with-envs] env ready, starting: $*"
exec -- "$@"
