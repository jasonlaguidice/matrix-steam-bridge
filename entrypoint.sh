#!/bin/sh
set -e

# Check if running as root and switch to bridge user
if [ "$(id -u)" = "0" ]; then
    echo "Running as root, switching to bridge user"
    exec su-exec bridge "$0" "$@"
fi

# Ensure data directory is mounted
if [ ! -d "/app/data" ]; then
    echo "Error: /app/data does not exist."
    echo "Please create a mount for it."
    exit 1
fi

# Ensure data is writable
if [ ! -w "/app/data" ]; then
    echo "Error: /app/data is not writable by bridge user"
    echo "Ensure it has write permissions for user with ID $(id -u)"
    exit 1
fi

if [ ! -w "/app/logs" ]; then
    echo "Error: /app/logs is not writable by bridge user"
    echo "Ensure it has write permissions for user with ID $(id -u)"
    exit 1
fi

echo "Using Config-file at ${CONFIG_FILE}"
echo "Using Registration-file at ${REGISTRATION_FILE}"

# Ensure config directory is mounted
CONFIG_DIR="$(dirname "${CONFIG_FILE}")"

if [ ! -d "${CONFIG_DIR}" ]; then
    echo "Error: Configuration directory ${CONFIG_DIR} does not exist."
    echo "Please create a mount for it or change the environment variable CONFIG_FILE"
    exit 1
fi


# Check if config exists
if [ ! -f "${CONFIG_FILE}" ]; then
    echo "No config file found at ${CONFIG_FILE}"
    echo "Generating one for you"

    if [ ! -w "${CONFIG_DIR}" ]; then
        echo "Error: Configuration directory ${CONFIG_DIR} not writeable by bridge user."
        echo "Ensure it has write permissions for user with ID $(id -u)"
        exit 1
    fi

    /app/steam -e -c "${CONFIG_FILE}"
    echo "Edit the config file and restart the container to create the registration."
    exit 0
fi

# Check if registration directory is mounted
REGISTRATION_DIR="$(dirname "${REGISTRATION_FILE}")"

if [ ! -d "${REGISTRATION_DIR}" ]; then
    echo "Error: Directory ${REGISTRATION_DIR} for the registration file does not exist."
    echo "Please create a mount for it or change the environment variable REGISTRATION_FILE"
    exit 1
fi

if [ ! -f "${REGISTRATION_FILE}" ]; then
    echo "Generating registration..."

    if [ ! -w "${REGISTRATION_DIR}" ]; then
        echo "Error: Directory ${REGISTRATION_DIR} for the registration file is not writable by bridge user"
        echo "Ensure it has write permissions for user with ID $(id -u)"
        exit 1
    fi
    
    /app/steam -g -c "${CONFIG_FILE}" -r "${REGISTRATION_FILE}"
    echo "Use the file to register the bridge with your homeserver, restart your homeserver and then start the container again"
    exit 0
fi

# Start the bridge
echo "Starting Matrix Steam Bridge..."

# Check if config needs steam_bridge_path fix for Docker
if grep -q "steam_bridge_path: ./SteamBridge" "${CONFIG_FILE}" 2>/dev/null; then
    echo "Fixing steam_bridge_path for Docker environment..."
    sed -i 's|steam_bridge_path: ./SteamBridge|steam_bridge_path: /app/steamkit-service|g' "${CONFIG_FILE}"
fi

exec /app/steam -c "${CONFIG_FILE}" "$@"
