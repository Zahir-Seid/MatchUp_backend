@echo off
REM Starts the API, admin API and worker, each in its own window.
REM The binaries load .env themselves, so no variables are set here.
setlocal
cd /d "%~dp0"

where go >nul 2>nul
if errorlevel 1 (
	echo go is not on PATH.
	exit /b 1
)

if not exist ".env" echo warning: .env not found; relying on the existing environment.

start "MatchUp api" cmd /k go run ./cmd/api
start "MatchUp admin" cmd /k go run ./cmd/admin
start "MatchUp worker" cmd /k go run ./cmd/worker

endlocal
