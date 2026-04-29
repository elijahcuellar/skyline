#!/usr/bin/env bash
set -euo pipefail
IFS=$'\n\t'

# Generate module signing keys
sudo /usr/sbin/kmodgenca -a

# Import the generated MOK key. Password is set to 'password'
KEY_PATH=/etc/pki/akmods/certs/public_key.der

# Only attempt import if mokutil is usable and key file exists
if command -v mokutil >/dev/null 2>&1 && mokutil --sb-state >/dev/null 2>&1; then
    if [ -f "$KEY_PATH" ]; then
        # Import the key non-interactively. Supplying the password twice (first for confirm).
        printf '%s\n%s\n' 'password' 'password' | sudo mokutil --import "$KEY_PATH" || {
            printf 'MOK import failed\n' >&2
        }
    else
        printf 'MOK key not found at %s; skipping import\n' "$KEY_PATH" >&2
    fi
fi
