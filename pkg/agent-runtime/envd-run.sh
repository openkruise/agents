#!/bin/bash
set -e  # Enable exit on error for the entire script

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

    # Method 2: adduser (Alpine, BusyBox style)
    if ! $user_created && command -v adduser &>/dev/null; then
        # Alpine/BusyBox adduser syntax
        if adduser -D -s "$default_shell" -h "$home_dir" "$username" 2>/dev/null; then
            user_created=true
            echo "User created using adduser."
        fi
    fi

    # Method 3: Direct manipulation of /etc/passwd (fallback)
    if ! $user_created; then
        echo "Falling back to direct /etc/passwd manipulation..."
        # Find next available UID (starting from 1000)
        local uid=1000
        while grep -q ":$uid:" /etc/passwd 2>/dev/null; do
            uid=$((uid + 1))
        done
        local gid=$uid

        # Create group if /etc/group exists
        if [ -f /etc/group ]; then
            if ! grep -q "^$username:" /etc/group 2>/dev/null; then
                echo "$username:x:$gid:" >> /etc/group
            else
                gid=$(grep "^$username:" /etc/group | cut -d: -f3)
            fi
        fi

        # Add user to /etc/passwd
        echo "$username:x:$uid:$gid:$username:$home_dir:$default_shell" >> /etc/passwd

        # Add entry to /etc/shadow if it exists
        if [ -f /etc/shadow ]; then
            echo "$username:!::::::" >> /etc/shadow 2>/dev/null || true
        fi

        user_created=true
        echo "User created via direct file manipulation."
    fi

    if ! $user_created; then
        echo "Warning: Failed to create user '$username'." >&2
        return 1
    fi

    # Create home directory
    mkdir -p "$home_dir"

    # Try to add user to sudo/wheel group (optional, don't fail if not possible)
    local sudo_group=""
    if getent group sudo &>/dev/null 2>&1 || grep -q "^sudo:" /etc/group 2>/dev/null; then
        sudo_group="sudo"
    elif getent group wheel &>/dev/null 2>&1 || grep -q "^wheel:" /etc/group 2>/dev/null; then
        sudo_group="wheel"
    fi

    if [ -n "$sudo_group" ]; then
        if command -v usermod &>/dev/null; then
            usermod -aG "$sudo_group" "$username" 2>/dev/null || true
        elif command -v addgroup &>/dev/null; then
            addgroup "$username" "$sudo_group" 2>/dev/null || true
        else
            # Direct manipulation: add user to group in /etc/group
            if [ -f /etc/group ]; then
                sed -i "s/^\($sudo_group:.*\)/\1,$username/" /etc/group 2>/dev/null || true
            fi
        fi
        echo "Added user to '$sudo_group' group."
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
            echo "$sudoers_line" >> /etc/sudoers 2>/dev/null || true
            echo "Added user to sudoers."
        fi
    elif [ -d /etc/sudoers.d ]; then
        # Some systems use sudoers.d directory
        echo "$username ALL=(ALL:ALL) NOPASSWD: ALL" > /etc/sudoers.d/$username 2>/dev/null || true
        chmod 440 /etc/sudoers.d/$username 2>/dev/null || true
        echo "Added user to sudoers.d."
    else
        echo "No sudoers configuration found, skipping sudo setup."
    fi

    # Set permissions on home directory
    chmod 777 -R "$home_dir" 2>/dev/null || chmod 777 "$home_dir" 2>/dev/null || true
    chown -R "$username:$username" "$home_dir" 2>/dev/null || chown "$username" "$home_dir" 2>/dev/null || true

    echo "User '$username' setup completed."
    return 0
}

# Execute user creation (don't let it fail the entire script)
create_user || echo "Warning: User creation encountered issues, but continuing..." >&2

# Start Envd
export GODEBUG=multipathtcp=0
nohup $ENVD_DIR/envd > /proc/1/fd/1 2>&1 &
