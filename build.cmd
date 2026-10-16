@echo off
rem Build the opencodehub c-shared plugin on Windows.
rem
rem   build.cmd [output-dir]
rem
rem Requires a MinGW-w64 gcc on PATH, or set CC to its full path:
rem   set CC=C:\path\to\mingw64\bin\gcc.exe
rem
rem The output file name must stay "opencodehub.dll": CLIProxyAPI matches the
rem plugin id against the DLL base name and the plugins.configs key.
setlocal

set "OUT_DIR=%~1"
if "%OUT_DIR%"=="" set "OUT_DIR=."

set CGO_ENABLED=1
if "%CC%"=="" (
  where gcc >nul 2>nul
  if errorlevel 1 (
    echo ERROR: no C compiler found. Install MinGW-w64 and put gcc on PATH,
    echo        or set CC to its full path, e.g.
    echo          set CC=C:\path\to\mingw64\bin\gcc.exe
    exit /b 1
  )
)

go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o "%OUT_DIR%\opencodehub.dll" .
if errorlevel 1 exit /b 1

echo built %OUT_DIR%\opencodehub.dll
endlocal
