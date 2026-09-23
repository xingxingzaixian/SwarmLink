@echo off
rem ============================================================================
rem  ASCII-ONLY WRAPPER -- do not put any non-ASCII character in this file.
rem
rem  Why: cmd.exe decodes batch files using the *console* code page (936/GBK on
rem  Chinese Windows). A UTF-8 file read as GBK loses byte alignment mid-line,
rem  so a line gets cut in half and its tail is executed as a command. The
rem  symptoms look like this and have nothing to do with the tools:
rem      'ev.bat' is not recognized as an internal or external command
rem      '/config.yml' is not recognized as an internal or external command
rem  Putting "chcp 65001" first does NOT fix it reliably: cmd reads ahead, so
rem  the bytes are already decoded with the old code page.
rem
rem  The real launcher is start_dev.ps1 (UTF-8 with BOM), where Chinese text is
rem  safe -- PowerShell reads its own script as UTF-8 and writes console output
rem  in the console's own code page. Keep this file free of non-ASCII forever.
rem ============================================================================
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0start_dev.ps1" %*
exit /b %ERRORLEVEL%
