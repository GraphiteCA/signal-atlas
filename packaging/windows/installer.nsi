; Signal Atlas's Windows installer (NSIS). scripts/release.py builds it in the Linux build container:
;
;   makensis -DVERSION=0.2.0 -DVERSION4=0.2.0.0 -DSRC=<folder with signal-atlas.exe, README.md,
;            NOTICE.md, LICENSE> -DICON=<icon.ico> -DOUT=<setup.exe> installer.nsi
;
; It installs for the current user only (no administrator prompt) into
; %LOCALAPPDATA%\Programs\Signal Atlas, adds Start-menu and desktop shortcuts and an entry in
; Settings > Apps, and leaves the user's data (%APPDATA%\Signal Atlas) alone when uninstalling.
; With /S it installs silently and starts the new version: the app's own updater uses that.

Unicode true
SetCompressor /SOLID lzma
RequestExecutionLevel user

!define APP "Signal Atlas"
!define EXE "signal-atlas.exe"
!define UNINST "Software\Microsoft\Windows\CurrentVersion\Uninstall\SignalAtlas"
; WebView2's runtime, per machine or per user
!define WV2 "{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}"

Name "${APP}"
Caption "${APP} ${VERSION} setup"
OutFile "${OUT}"
InstallDir "$LOCALAPPDATA\Programs\${APP}"
InstallDirRegKey HKCU "${UNINST}" "InstallLocation"
BrandingText "${APP} ${VERSION}"

VIProductVersion "${VERSION4}"
VIAddVersionKey "ProductName" "${APP}"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "FileDescription" "${APP} installer"
VIAddVersionKey "LegalCopyright" "MIT License, the Signal Atlas contributors"

!include "MUI2.nsh"
!define MUI_ICON "${ICON}"
!define MUI_UNICON "${ICON}"
!define MUI_ABORTWARNING
!define MUI_FINISHPAGE_RUN "$INSTDIR\${EXE}"
!define MUI_FINISHPAGE_RUN_TEXT "Open ${APP}"
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_LICENSE "${SRC}\LICENSE"
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Function .onInit
  ; the screens are drawn by WebView2: Windows 11 has it, an older Windows 10 may not
  ReadRegStr $0 HKLM "SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\${WV2}" "pv"
  StrCmp $0 "" 0 have
  ReadRegStr $0 HKLM "SOFTWARE\Microsoft\EdgeUpdate\Clients\${WV2}" "pv"
  StrCmp $0 "" 0 have
  ReadRegStr $0 HKCU "Software\Microsoft\EdgeUpdate\Clients\${WV2}" "pv"
  StrCmp $0 "" 0 have
  IfSilent have
  MessageBox MB_YESNO|MB_ICONINFORMATION "${APP} needs Microsoft's WebView2 Runtime, which this PC doesn't have yet.$\r$\n$\r$\nOpen Microsoft's download page now? Install it, then run this setup again." IDNO have
  ExecShell "open" "https://developer.microsoft.com/microsoft-edge/webview2/#download"
  Abort
  have:
FunctionEnd

Section "Install"
  SetOutPath "$INSTDIR"
  ; an update starts while the old copy is closing: wait for its program file to come free
  StrCpy $1 0
  wait:
    ClearErrors
    IfFileExists "$INSTDIR\${EXE}" 0 free
    Delete "$INSTDIR\${EXE}"
    IfErrors 0 free
    IntOp $1 $1 + 1
    IntCmp $1 40 0 0 locked
    Sleep 250
    Goto wait
  locked:
    MessageBox MB_OK|MB_ICONSTOP "${APP} is still running. Close it and run this setup again."
    Abort
  free:
  File "${SRC}\${EXE}"
  File "${SRC}\README.md"
  File "${SRC}\NOTICE.md"
  File "${SRC}\LICENSE"
  WriteUninstaller "$INSTDIR\uninstall.exe"
  CreateShortcut "$SMPROGRAMS\${APP}.lnk" "$INSTDIR\${EXE}"
  CreateShortcut "$DESKTOP\${APP}.lnk" "$INSTDIR\${EXE}"
  WriteRegStr HKCU "${UNINST}" "DisplayName" "${APP}"
  WriteRegStr HKCU "${UNINST}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKCU "${UNINST}" "Publisher" "Signal Atlas contributors"
  WriteRegStr HKCU "${UNINST}" "DisplayIcon" "$INSTDIR\${EXE}"
  WriteRegStr HKCU "${UNINST}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINST}" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegStr HKCU "${UNINST}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
  WriteRegStr HKCU "${UNINST}" "URLInfoAbout" "https://github.com/olosnoks/signal-atlas"
  WriteRegDWORD HKCU "${UNINST}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINST}" "NoRepair" 1
  ; a silent install is an update: open the new version
  IfSilent 0 done
  Exec '"$INSTDIR\${EXE}"'
  done:
SectionEnd

Section "Uninstall"
  Delete "$INSTDIR\${EXE}"
  Delete "$INSTDIR\README.md"
  Delete "$INSTDIR\NOTICE.md"
  Delete "$INSTDIR\LICENSE"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\${APP}.lnk"
  Delete "$DESKTOP\${APP}.lnk"
  DeleteRegKey HKCU "${UNINST}"
  ; the explorer's store, Drive database settings and the map cache stay in %APPDATA%\Signal Atlas
SectionEnd
