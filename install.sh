#!/bin/bash
# install.sh — Install qoder-bridge as a systemd service
# Run: bash install.sh
set -e

BRIDGE_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN="$HOME/.local/bin/qoder-bridge"

echo "Installing qoder-bridge..."

# Read port from .env (default 7100)
PORT=7100
if [ -f "$BRIDGE_DIR/.env" ]; then
    ENV_PORT=$(grep -E "^QODER_PORT=" "$BRIDGE_DIR/.env" | cut -d= -f2 | tr -d ' "')
    if [ -n "$ENV_PORT" ]; then
        PORT="$ENV_PORT"
    fi
fi
echo "  port: $PORT"

# Build
echo "  building..."
cd "$BRIDGE_DIR"
go build -o qoder-bridge . 2>/dev/null || { echo "FAILED: go build"; exit 1; }

# Stop running service first to release binary
if [ "$(id -u)" -eq 0 ]; then
    if systemctl is-active --quiet qoder-bridge 2>/dev/null; then
        echo "  stopping service..."
        systemctl stop qoder-bridge
        sleep 2
    fi
else
    if systemctl --user is-active --quiet qoder-bridge 2>/dev/null; then
        echo "  stopping service..."
        systemctl --user stop qoder-bridge
        sleep 2
    fi
fi

# Copy binary — atomic rename to avoid "Text file busy"
echo "  installing binary to $BIN..."
mkdir -p "$HOME/.local/bin"
cp qoder-bridge "$BIN.tmp"
mv -f "$BIN.tmp" "$BIN"
chmod +x "$BIN"

# Create .env if missing
if [ ! -f "$BRIDGE_DIR/.env" ]; then
    echo "  creating .env from .env.example..."
    cp "$BRIDGE_DIR/.env.example" "$BRIDGE_DIR/.env"
    echo "  EDIT $BRIDGE_DIR/.env AND ADD YOUR PATs!"
fi

# Detect root vs normal user
if [ "$(id -u)" -eq 0 ]; then
    # Root: use system-level service
    SERVICE_DIR="/etc/systemd/system"
    SERVICE_FILE="$SERVICE_DIR/qoder-bridge.service"
    echo "  creating system service (root)..."

    cat > "$SERVICE_FILE" << EOF
[Unit]
Description=Qoder Bridge — OpenAI-compatible proxy for Qoder PATs
After=network.target

[Service]
Type=simple
ExecStart=$BIN run -env $BRIDGE_DIR/.env
Restart=on-failure
RestartSec=5
WorkingDirectory=$BRIDGE_DIR

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable qoder-bridge
    systemctl restart qoder-bridge

    sleep 3

    if systemctl is-active --quiet qoder-bridge; then
        echo ""
        echo "qoder-bridge installed and running!"
        echo "  port:    127.0.0.1:$PORT"
        echo "  status:  qoder-bridge status"
        echo "  logs:    journalctl -u qoder-bridge -f"
        echo "  stop:    qoder-bridge stop"
        echo "  update:  qoder-bridge update"
    else
        echo ""
        echo "qoder-bridge failed to start. Check logs:"
        echo "  journalctl -u qoder-bridge --since '30 sec ago'"
    fi
else
    # Normal user: use user-level service
    SERVICE_DIR="$HOME/.config/systemd/user"
    SERVICE_FILE="$SERVICE_DIR/qoder-bridge.service"
    echo "  creating user service..."

    mkdir -p "$SERVICE_DIR"
    cat > "$SERVICE_FILE" << EOF
[Unit]
Description=Qoder Bridge — OpenAI-compatible proxy for Qoder PATs
After=network.target

[Service]
Type=simple
ExecStart=$BIN run -env $BRIDGE_DIR/.env
Restart=on-failure
RestartSec=5
WorkingDirectory=$BRIDGE_DIR

[Install]
WantedBy=default.target
EOF

    systemctl --user daemon-reload
    systemctl --user enable qoder-bridge
    systemctl --user restart qoder-bridge

    sleep 3

    if systemctl --user is-active --quiet qoder-bridge; then
        echo ""
        echo "qoder-bridge installed and running!"
        echo "  port:    127.0.0.1:$PORT"
        echo "  status:  qoder-bridge status"
        echo "  logs:    journalctl --user -u qoder-bridge -f"
        echo "  stop:    qoder-bridge stop"
        echo "  update:  qoder-bridge update"
    else
        echo ""
        echo "qoder-bridge failed to start. Check logs:"
        echo "  journalctl --user -u qoder-bridge --since '30 sec ago'"
    fi
fi
