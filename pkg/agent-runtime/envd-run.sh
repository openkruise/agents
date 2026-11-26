#!/bin/bash
set -e  # Enable exit on error for the entire script

# Usage: envd-run.sh [OPTIONS] [HELPER_FLAGS...] [-- <cmd> [args...]]
#
# Injected postStart hook of the business container: it creates the E2B-style
# "user" account when running as root, launches the data plane (envd,
# agent-helper, or agent-runtime) in the background, then runs an optional
# user-init command. Run `envd-run.sh --help` for the authoritative flag
# reference.
NOLOG=false
HELPER_MODE=false
LOCAL_MODE=false
USER_INIT_ARGS=()
FORWARD_ARGS=()
PASSTHROUGH_ARGS=()
while [ $# -gt 0 ]; do
    case "$1" in
        --helper)
            HELPER_MODE=true
            shift
            ;;
        --local)
            LOCAL_MODE=true
            shift
            ;;
        --nolog)
            NOLOG=true
            shift
            ;;
        --)
            # Everything after -- becomes the user-init command (no shell interpretation)
            shift
            USER_INIT_ARGS=("$@")
            break
            ;;
        --help|-h)
            echo "Usage: envd-run.sh [OPTIONS] [HELPER_FLAGS...] [-- <cmd> [args...]]"
            echo "  --helper             Launch agent-helper (helper mode data plane) instead of envd;"
            echo "                       skips default user creation"
            echo "  --local              Launch agent-runtime in local mode (--runtime-mode=local) instead of envd"
            echo "  --nolog              Suppress runtime stdout/stderr (redirect to /dev/null)"
            echo "  FLAGS...             In --helper/--local mode, other flags before -- are forwarded to the"
            echo "                       launched binary (e.g. --helper-socket, --allowed-peer-uid, --port)"
            echo "  -- <cmd> [args...]   Run user-init command directly without shell interpretation"
            echo "  --help               Show this help message"
            echo ""
            echo "Runtime output is piped to PID 1's stdout so container log collectors pick it up."
            echo "When that is not writable (non-root business container), output goes to a bounded"
            echo "per-binary log file under \$ENVD_DIR instead."
            exit 0
            ;;
        *)
            # Not a recognized script flag. In helper/local mode these are forwarded
            # to the launched binary; in envd mode they remain errors. Ordering
            # relative to --helper/--local does not matter because validation
            # happens after parsing.
            PASSTHROUGH_ARGS+=("$1")
            shift
            ;;
    esac
done

# --helper and --local select mutually exclusive deployment modes.
if [ "$HELPER_MODE" = "true" ] && [ "$LOCAL_MODE" = "true" ]; then
    echo "Error: --helper and --local are mutually exclusive" >&2
    exit 1
fi

# Resolve pass-through flags. In helper/local mode they are forwarded verbatim to
# the launched binary (agent-helper / agent-runtime). In the default envd mode any
# unrecognized flag stays an error, preserving the previous strict behaviour.
if [ "$HELPER_MODE" = "true" ] || [ "$LOCAL_MODE" = "true" ]; then
    FORWARD_ARGS=("${PASSTHROUGH_ARGS[@]}")
elif [ ${#PASSTHROUGH_ARGS[@]} -gt 0 ]; then
    echo "Error: unknown argument '${PASSTHROUGH_ARGS[0]}'" >&2
    exit 1
fi

# Check if ENVD_DIR environment variable is defined
if [ -z "$ENVD_DIR" ]; then
    echo "Error: ENVD_DIR environment variable is not defined" >&2
    exit 1
fi

# Create default user
create_user() {
    local username="user"
    local home_dir="/home/$username"
    local default_shell="/bin/bash"

    # If user already exists, skip creation
    if id "$username" &>/dev/null; then
        echo "User '$username' already exists, skipping creation."
        return 0
    fi

    # Determine available shell
    if [ ! -x "$default_shell" ]; then
        if [ -x "/bin/sh" ]; then
            default_shell="/bin/sh"
        else
            default_shell="/bin/ash"  # Alpine uses ash
        fi
    fi

    echo "Creating user '$username' with shell '$default_shell'..."

    # Try different methods to create user
    local user_created=false

    # Method 1: useradd (most Linux distros)
    if ! $user_created && command -v useradd &>/dev/null; then
        if useradd -m -s "$default_shell" "$username" 2>/dev/null; then
            user_created=true
            echo "User created using useradd."
        fi
    fi

    # Method 2: adduser. BusyBox and Debian adduser have incompatible flag
    # sets: -D/-h mean "no password"/"home" in BusyBox, but in Debian adduser
    # -D is invalid and -h is an alias of --help, which prints usage and exits
    # 0 without creating anything, falsely reporting success. Detect the
    # binary identity (BusyBox adduser is an applet symlink to the busybox
    # binary; Debian's is a standalone script) instead of matching help prose.
    if ! $user_created && command -v adduser &>/dev/null; then
        case "$(readlink -f "$(command -v adduser)" 2>/dev/null)" in
            */busybox)
                # BusyBox / Alpine adduser syntax
                if adduser -D -s "$default_shell" -h "$home_dir" "$username" 2>/dev/null; then
                    user_created=true
                    echo "User created using BusyBox adduser."
                fi
                ;;
            *)
                # Debian adduser syntax
                if adduser --disabled-password --gecos "" --shell "$default_shell" --home "$home_dir" "$username" 2>/dev/null; then
                    user_created=true
                    echo "User created using Debian adduser."
                fi
                ;;
        esac
    fi

    if ! $user_created; then
        echo "Warning: Failed to create user '$username'." >&2
        return 1
    fi

    # Create home directory
    mkdir -p "$home_dir"

    # Try to add user to sudo/wheel group (optional, don't fail if not possible)
    local sudo_group=""
    if getent group sudo &>/dev/null || grep -q "^sudo:" /etc/group 2>/dev/null; then
        sudo_group="sudo"
    elif getent group wheel &>/dev/null || grep -q "^wheel:" /etc/group 2>/dev/null; then
        sudo_group="wheel"
    fi

    if [ -n "$sudo_group" ]; then
        local group_added=false
        if command -v usermod &>/dev/null; then
            usermod -aG "$sudo_group" "$username" 2>/dev/null && group_added=true
        elif command -v addgroup &>/dev/null; then
            addgroup "$username" "$sudo_group" 2>/dev/null && group_added=true
        else
            # Direct manipulation: add user to group in /etc/group. sed -i
            # exits 0 even when the substitution matches nothing, so verify
            # membership afterwards instead of trusting sed's exit code.
            if [ -f /etc/group ]; then
                sed -i "s/^\($sudo_group:.*\)/\1,$username/" /etc/group 2>/dev/null \
                    && grep "^$sudo_group:" /etc/group 2>/dev/null | grep -qw "$username" \
                    && group_added=true
            fi
        fi
        if $group_added; then
            echo "Added user to '$sudo_group' group."
        else
            echo "Warning: Failed to add user to '$sudo_group' group." >&2
        fi
    else
        echo "No sudo/wheel group found, skipping group assignment."
    fi

    # Remove password (optional, don't fail)
    if command -v passwd &>/dev/null; then
        passwd -d "$username" 2>/dev/null || true
    elif [ -f /etc/shadow ]; then
        # Direct manipulation: set empty password in shadow
        sed -i "s/^$username:[^:]*:/$username:::/" /etc/shadow 2>/dev/null || true
    fi

    # Configure sudoers if sudoers file exists and sudo is available
    if [ -f /etc/sudoers ]; then
        local sudoers_line="$username ALL=(ALL:ALL) NOPASSWD: ALL"
        if ! grep -qF "$sudoers_line" /etc/sudoers 2>/dev/null; then
            if echo "$sudoers_line" >> /etc/sudoers 2>/dev/null; then
                echo "Added user to sudoers."
            else
                echo "Warning: Failed to add user to sudoers." >&2
            fi
        fi
    elif [ -d /etc/sudoers.d ]; then
        # Some systems use sudoers.d directory
        if echo "$username ALL=(ALL:ALL) NOPASSWD: ALL" > /etc/sudoers.d/$username 2>/dev/null; then
            chmod 440 /etc/sudoers.d/$username 2>/dev/null || true
            echo "Added user to sudoers.d."
        else
            echo "Warning: Failed to add user to sudoers.d." >&2
        fi
    else
        echo "No sudoers configuration found, skipping sudo setup."
    fi

    # Set permissions on home directory
    chmod 777 -R "$home_dir" 2>/dev/null || chmod 777 "$home_dir" 2>/dev/null || true
    chown -R "$username:$username" "$home_dir" 2>/dev/null || chown "$username" "$home_dir" 2>/dev/null || true

    echo "User '$username' setup completed."
    return 0
}

# Execute user creation (don't let it fail the entire script).
# Skipped in helper mode: agent-helper derives its identity from its own runtime
# uid (see resolveDefaultUser) and does not need the E2B-style "user" account, so
# creating it in the business container is unnecessary and would only emit noise
# on non-root containers.
# Also skipped when not running as root: every step of create_user (useradd/
# adduser, mkdir /home, usermod, sudoers writes, chown) requires root and fails
# noisily in non-root business containers (e.g. OpenClaw runs as 'node'), where
# the E2B-style "user" account is not needed either.
if [ "$HELPER_MODE" = "true" ]; then
    echo "[helper] Skipping default user creation (not required in helper mode)."
elif [ "$(id -u)" -ne 0 ]; then
    echo "Skipping default user creation (not running as root)."
else
    create_user || echo "Warning: User creation encountered issues, but continuing..." >&2
fi

# Start the runtime process. Select the binary by mode:
#   helper -> in-container agent-helper (helper mode data plane)
#   local  -> agent-runtime with --runtime-mode=local (native execution)
#   envd   -> the default envd runtime
# It is backgrounded so the user-init hook below can still run afterwards.
export GODEBUG=multipathtcp=0
if [ "$HELPER_MODE" = "true" ]; then
    RUN_CMD=("$ENVD_DIR/agent-helper" "${FORWARD_ARGS[@]}")
elif [ "$LOCAL_MODE" = "true" ]; then
    RUN_CMD=("$ENVD_DIR/agent-runtime" --runtime-mode=local --isnotfc "${FORWARD_ARGS[@]}")
else
    RUN_CMD=("$ENVD_DIR/envd" --isnotfc)
fi

# Bounds for the non-root log-file fallback (env-overridable for tuning/tests):
#   MAX_RUNTIME_LOG_BYTES      truncate once the log grows past this (default 50 MiB)
#   KEEP_RUNTIME_LOG_BYTES     keep the tail of this size after truncation (default 10 MiB)
#   RUNTIME_LOG_CHECK_INTERVAL seconds between size checks (default 60)
MAX_RUNTIME_LOG_BYTES=${MAX_RUNTIME_LOG_BYTES:-52428800}
KEEP_RUNTIME_LOG_BYTES=${KEEP_RUNTIME_LOG_BYTES:-10485760}
RUNTIME_LOG_CHECK_INTERVAL=${RUNTIME_LOG_CHECK_INTERVAL:-60}

# Truncate the fallback log to its tail in place. Rewriting through the same
# inode keeps the runtime's O_APPEND fd valid, whereas swapping in a new file
# would strand the writer on the old inode. The caller must redirect this
# function's output: a backgrounded child that keeps the postStart hook's IO
# pipe open would hang containerd's ExecSync drain and fail the hook.
rotate_runtime_log() {
    local runtime_pid=$1
    local runtime_log=$2
    while kill -0 "$runtime_pid" 2>/dev/null; do
        sleep "$RUNTIME_LOG_CHECK_INTERVAL"
        local size
        size=$(wc -c < "$runtime_log" 2>/dev/null || echo 0)
        [ "$size" -gt "$MAX_RUNTIME_LOG_BYTES" ] || continue
        if tail -c "$KEEP_RUNTIME_LOG_BYTES" "$runtime_log" > "$runtime_log.tmp" 2>/dev/null; then
            cat "$runtime_log.tmp" > "$runtime_log" 2>/dev/null || true
            rm -f "$runtime_log.tmp"
        fi
    done
}

# Select the output target once and launch once. The default pipes runtime
# output to PID 1's stdout so container log collectors pick it up; /proc/1/fd/1
# only opens for processes sharing PID 1's uid (root), and in non-root business
# containers a failed redirect would abort the launch silently. Fall back to a
# bounded per-binary log file under $ENVD_DIR (writable by the business
# container, readable by the root sidecar). Every backgrounded child redirects
# stdout+stderr so the postStart hook's IO pipe reaches EOF when the hook exits.
rotate=false
if [ "$NOLOG" = "true" ]; then
    # Suppress runtime stdout/stderr entirely when invoked with --nolog.
    log_target=/dev/null
elif (exec 3>>/proc/1/fd/1) 2>/dev/null; then
    log_target=/proc/1/fd/1
elif [ -w "$ENVD_DIR" ]; then
    log_target="${ENVD_DIR}/$(basename "${RUN_CMD[0]}").log"
    rotate=true
    echo "Warning: /proc/1/fd/1 is not writable; runtime output goes to $log_target" >&2
else
    log_target=/dev/null
    echo "Warning: /proc/1/fd/1 and $ENVD_DIR are not writable; runtime output goes to /dev/null" >&2
fi
nohup "${RUN_CMD[@]}" >>"$log_target" 2>&1 &
if $rotate; then
    rotate_runtime_log "$!" "$log_target" >>"$log_target" 2>&1 &
fi

# User init hook: everything after "--" runs directly (no shell interpretation,
# kubectl exec style). A non-zero exit code aborts startup to surface hook
# failures.
if [ ${#USER_INIT_ARGS[@]} -gt 0 ]; then
    echo "[user-init] Running direct exec: ${USER_INIT_ARGS[*]}"
    "${USER_INIT_ARGS[@]}"
fi
