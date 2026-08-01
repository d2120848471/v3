@echo off
rem Build the Windows standalone distribution by double-clicking this file.
rem PowerShell blocks .ps1 execution by default, so bypass the policy for this call.
setlocal
cd /d "%~dp0.."
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0build_windows.ps1" %*
echo.
pause
