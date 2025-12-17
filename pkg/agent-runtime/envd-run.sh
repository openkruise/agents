#!/bin/bash
set -e  # Enable exit on error for the entire script

# Check if ENVD_DIR environment variable is defined
if [ -z "$ENVD_DIR" ]; then
    echo "Error: ENVD_DIR environment variable is not defined" >&2
    exit 1
fi

# Create default user
if ! id "user" &>/dev/null; then
    useradd -ms /bin/bash user
    usermod -aG sudo user
    passwd -d -q user
    echo "user ALL=(ALL:ALL) NOPASSWD: ALL" >>/etc/sudoers
    mkdir -p /home/user
    chmod 777 -R /home/user
    chown -R user:user /home/user
fi

# Start Envd
export GODEBUG=multipathtcp=0
nohup $ENVD_DIR/envd > /proc/1/fd/1 2>&1 &
