@echo off
rem Workass tools launcher (Windows). Runs the tools client with the bundled
rem OpenJS-signed node.exe so no unsigned Workass executable is ever launched
rem as a short-lived CLI child. Usage: workass-tools.cmd tools list or tools call ...
setlocal DisableDelayedExpansion
"%~dp0..\..\node\windows-amd64\node.exe" "%~dp0workass-tools.mjs" %*
exit /b %ERRORLEVEL%
