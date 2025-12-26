@echo off
REM Production launcher for Finn Desktop Daemon (Windows)

REM Set production environment variables
set FINN_RELAY_URL=wss://api.tryfinn.ai/ws
set FINN_DASHBOARD_URL=https://tryfinn.ai

REM Launch daemon
finn.exe
