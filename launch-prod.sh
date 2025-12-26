#!/bin/bash
# Production launcher for Finn Desktop Daemon

# Set production environment variables
export FINN_RELAY_URL="wss://api.tryfinn.ai/ws"
export FINN_DASHBOARD_URL="https://tryfinn.ai"

# Launch daemon
./finn
