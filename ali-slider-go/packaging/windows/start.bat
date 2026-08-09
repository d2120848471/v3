@echo off
setlocal
chcp 65001 >nul
cd /d "%~dp0"
title Ali Slider Go

if not exist "ali-slider-go.exe" (
  echo 错误：当前目录没有 ali-slider-go.exe。
  echo 请先完整解压 ZIP，再双击 start.bat。
  pause
  exit /b 1
)

if not exist "ali_slider_v8_runtime.dll" (
  echo 错误：当前目录没有设备与动态 PE 运行所需的 ali_slider_v8_runtime.dll。
  echo 请重新下载并完整解压便携包。
  pause
  exit /b 1
)

echo Ali Slider Go 正在启动。
echo 默认 API 测试页：http://127.0.0.1:8000/
echo 默认本机 API：http://127.0.0.1:8000/api/slider
echo 默认健康检查：http://127.0.0.1:8000/health
echo 如追加了 flag，请以启动日志中的实际监听地址为准。
echo 默认启动会预热设备会话，请等待 status=ready 日志。
echo 在本窗口按 Ctrl+C 可停止服务。
echo.

"%~dp0ali-slider-go.exe" ^
  --host=127.0.0.1 ^
  --port=8000 ^
  "--artifact-dir=%~dp0var\artifacts" ^
  "--v8-library=%~dp0ali_slider_v8_runtime.dll" ^
  %*

set "exit_code=%ERRORLEVEL%"
if not "%exit_code%"=="0" (
  echo.
  echo 服务异常退出，退出码：%exit_code%。
  echo 如果日志包含 address already in use，请关闭已启动实例或改用其他端口。
  pause
)
exit /b %exit_code%
