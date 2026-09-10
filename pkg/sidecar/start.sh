#!/bin/bash
# Starts both processes of the csi-sidecar-customfuse image:
#   - csi-mount-proxy-server: listens on /var/run/csi/mounter.sock, runs
#     /entrypoint.sh for each mount (s3fs/juicefs FUSE mount)
#   - csi-sidecar-customfuse: CSI node server listening on the per-driver
#     csi.sock, forwards NodePublishVolume to mount-proxy-server
#
# The image runs as root (no USER directive), so /var/run/csi is always
# writable; if the image is ever converted to a non-root user, an explicit
# volume mount or directory pre-creation becomes necessary here.
#
# Supervision requires bash >= 5.1 for wait -n -p, which blocks until a
# child exits, reaps it, and reports which PID was reaped: there is no
# zombie window, so no kill -0 guesswork is needed. Both runtime base
# images provide this: ubuntu:22.04 ships bash 5.1 and juicedata/mount
# ships bash 5.x. Do not switch the base images to CentOS 7 (bash 4.2)
# or Alpine (no bash); the version check below makes such a change fail
# loudly instead of misbehaving.
set -e
if [ "${BASH_VERSINFO[0]}" -lt 5 ] || { [ "${BASH_VERSINFO[0]}" -eq 5 ] && [ "${BASH_VERSINFO[1]}" -lt 1 ]; }; then
    echo "start.sh requires bash >= 5.1 for wait -n -p" >&2
    exit 1
fi
mkdir -p /var/run/csi
csi-mount-proxy-server --driver=customfuse &
MPID=$!
csi-sidecar-customfuse &
SPID=$!
TERMINATING=0
MAIN_PID=$BASHPID
# Forward termination signals to both children. The order does not
# matter: the node server's drain may fail in-flight forwards (the
# container is going away anyway), and mount-proxy's FUSE umount does
# not depend on the node server being alive — the mount namespace
# disappears with the container either way. The BASHPID guard makes
# the handler a no-op in subshells: a freshly forked subshell inherits
# this trap until it installs its own, and letting it re-kill MPID/SPID
# would be harmless but sloppy (and wrong if a PID got reused).
trap 'if [ "$BASHPID" = "$MAIN_PID" ]; then TERMINATING=1; kill $MPID $SPID 2>/dev/null || true; fi' TERM INT

# arm_kill sends TERM and arms a 10-second SIGKILL escalation. The timer
# subshell reaps its own sleep child on TERM, so no orphaned process or
# zombie survives. All wait calls stay at the top level: in bash 5.1,
# wait executed inside a function can miss the SIGCHLD of a child that
# died just after kill and then block until the next SIGCHLD arrives
# (the timer's, 10 seconds later), stalling shutdown by the full grace
# period. The timer PID travels back through the TIMER_PID global.
arm_kill() {
    _pid=$1
    kill $_pid 2>/dev/null || true
    # Separate kill from the caller's wait with a short sleep. bash 5.1
    # can miss a SIGCHLD that lands between kill and wait, which would
    # stall the next wait until the timer's SIGCHLD arrives 10 seconds
    # later. The sleep is also a fork+wait cycle that drains an already
    # pending SIGCHLD. A child that exits after this window is safe: a
    # SIGCHLD arriving while a wait is already blocked is handled
    # normally, as the main wait -n -p above demonstrates. The || true
    # keeps set -e from aborting when a second TERM interrupts the
    # sleep mid-run (the trap has already recorded TERMINATING).
    sleep 0.5 || true
    ( trap 'if [ -n "$SL" ]; then kill $SL 2>/dev/null; wait $SL 2>/dev/null; fi; exit 0' TERM
      sleep 10 &
      SL=$!
      wait $SL && kill -9 $_pid 2>/dev/null ) &
    TIMER_PID=$!
    # The timer subshell failed to fork (or died instantly). An empty
    # TIMER_PID makes timer_running report false, which sends the
    # caller into the immediate kill -9 branch.
    timer_running || TIMER_PID=""
}

# cancel_timer stops the armed escalation timer without waiting on it
# (all waits stay at the top level, see arm_kill). TERM alone is not
# enough: a subshell that has not yet installed its own trap inherits
# the parent shell's trap, whose handler returns and lets the subshell
# run out the full 10 seconds. The SIGKILL fallback after a short
# grace closes that window; in the rare window case it can leave the
# timer's own sleep child running for up to 10 seconds, which is
# harmless — the container is exiting and the sleep dies on its own.
cancel_timer() {
    # An empty TIMER_PID means arm_kill failed to fork the timer; there
    # is nothing to cancel.
    [ -n "$TIMER_PID" ] || return 0
    kill $TIMER_PID 2>/dev/null || true
    sleep 0.05 || true
    if timer_running; then
        kill -9 $TIMER_PID 2>/dev/null || true
    fi
}

# timer_running reports whether the escalation timer is still live. It
# reads /proc instead of using kill -0, which reports success for a
# zombie and would make a dead timer look alive (that in turn would
# send the caller into a wait that can never be satisfied if the
# target ignores TERM). The state and parent-PID fields are checked
# together: after the timer dies and the shell reaps it, a busy sibling
# (e.g. the target's own children) can reuse the freed PID within
# milliseconds, and a bare state check would mistake the new process
# for a live timer. read is a shell builtin, so this introduces no
# fork and no wait.
timer_running() {
    local _stat _rest
    [ -n "$TIMER_PID" ] || return 1
    IFS= read -r _stat 2>/dev/null < "/proc/$TIMER_PID/stat" || return 1
    _rest=${_stat##*) }
    [ "${_rest%% *}" != "Z" ] || return 1
    _rest=${_rest#* }
    [ "${_rest%% *}" = "$MAIN_PID" ]
}

# Block until either child exits, then stop the other and propagate the
# exit status. The -p report names the reaped child exactly, so a child
# that crashed at the same moment as its sibling is handled by the right
# branch. If mount-proxy dies first, the whole container fails so
# Kubernetes rebuilds the sandbox instead of silently degrading into
# "all new mounts fail". The if-context keeps set -e from treating a
# non-zero wait (child crash, or a wait interrupted by the TERM trap) as
# a fatal shell error before the cleanup below can run.
if wait -n -p REAPED $MPID $SPID; then
    FIRST_STATUS=0
else
    FIRST_STATUS=$?
fi
if [ "$REAPED" = "$MPID" ]; then
    # The proxy exited first. A start failure is already reported by the
    # shell ("command not found"); anything else is an unexpected crash.
    # If the TERM trap interrupted the wait above, REAPED may be unset;
    # falling into the branch below is still correct — both branches
    # exit 0 under TERMINATING.
    [ $TERMINATING -eq 0 ] && echo "csi-mount-proxy-server exited unexpectedly (status $FIRST_STATUS)" >&2
    arm_kill $SPID
    if timer_running; then
        wait $SPID 2>/dev/null || true
    else
        # The escalation timer failed to start: kill -9 directly so the
        # wait below cannot block forever on a TERM-ignoring child
        # (kubelet's grace-period SIGKILL would eventually reap the
        # container, but explicit escalation keeps shutdown within it).
        kill -9 $SPID 2>/dev/null || true
        wait $SPID 2>/dev/null || true
    fi
    cancel_timer
    # A guarded wait: with an empty TIMER_PID (fork failure), a bare
    # `wait` would wait for every remaining background job — including
    # the orphaned timer sleep — and delay shutdown by up to 10s.
    [ -n "$TIMER_PID" ] && wait $TIMER_PID 2>/dev/null || true
    if [ $TERMINATING -eq 1 ]; then
        exit 0
    fi
    exit $FIRST_STATUS
fi
# The node server exited, on its own or through the termination trap.
arm_kill $MPID
if timer_running; then
    wait $MPID 2>/dev/null || true
else
    kill -9 $MPID 2>/dev/null || true
    wait $MPID 2>/dev/null || true
fi
cancel_timer
[ -n "$TIMER_PID" ] && wait $TIMER_PID 2>/dev/null || true
if [ $TERMINATING -eq 1 ]; then
    exit 0
fi
exit $FIRST_STATUS
