; NSIS installer for RASA for Jellyfin.
;
; Unsigned by design (decision 13): there is no code-signing budget, so
; SmartScreen will warn on every download. That is documented on the Pages
; site rather than worked around.

!define APPNAME "RASA for Jellyfin"

; The release workflow passes -DVERSION=vX.Y.Z. The fallback keeps a local
; "makensis rasa.nsi" working for anyone testing the installer by hand.
!ifndef VERSION
  !define VERSION "0.0.0-dev"
!endif

Name "${APPNAME}"
OutFile "rasa-setup-windows-x64.exe"
InstallDir "$PROGRAMFILES64\RASA"
RequestExecutionLevel admin
Unicode true
ShowInstDetails show

Page directory
Page instfiles
UninstPage uninstConfirm
UninstPage instfiles

Section "Install"
  SetOutPath "$INSTDIR"
  File "rasa.exe"
  File "rasa-sync.exe"
  File "caddy.exe"

  ; The shared data directory lives outside INSTDIR on purpose: uninstalling
  ; must leave logs, state and the recovery file behind, which is exactly when
  ; they become most valuable (SPEC.md 15).
  CreateDirectory "$COMMONPROGRAMDATA\RASA"
  CreateDirectory "$COMMONPROGRAMDATA\RASA\logs"

  ; The firewall rule is scoped to the Caddy binary rather than to a port,
  ; because the listener port is not known until setup runs - and a
  ; program-scoped rule is tighter than opening ports outright. It is written
  ; by rasa.exe rather than here, because it has to name the copy of Caddy that
  ; actually runs: RASA copies both helper binaries into
  ; COMMONPROGRAMDATA\RASA\bin at first launch so that uninstalling INSTDIR
  ; does not take the running proxy with it, and a rule pointing at INSTDIR
  ; would stop matching the moment that happened.

  WriteUninstaller "$INSTDIR\uninstall.exe"
  CreateShortcut "$SMPROGRAMS\${APPNAME}.lnk" "$INSTDIR\rasa.exe"

  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RASA" "DisplayName" "${APPNAME}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RASA" "DisplayVersion" "${VERSION}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RASA" "UninstallString" "$INSTDIR\uninstall.exe"

  ; Launch in this session so the wizard inherits the privileges the installer
  ; already holds, and the user sees exactly one prompt.
  Exec '"$INSTDIR\rasa.exe"'
SectionEnd

Section "Uninstall"
  ; Only the wizard is removed. Caddy, the scheduled task and the data
  ; directory stay - removing them would take remote access down, which is the
  ; opposite of what uninstalling a setup app should mean here.
  ; Everything in INSTDIR goes, including the shipped copies of caddy.exe and
  ; rasa-sync.exe. The copies that run live in COMMONPROGRAMDATA\RASA\bin and
  ; are untouched by this, as are the firewall rule, the service and the
  ; scheduled task.
  Delete "$INSTDIR\rasa.exe"
  Delete "$INSTDIR\rasa-sync.exe"
  Delete "$INSTDIR\caddy.exe"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\${APPNAME}.lnk"
  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RASA"

  MessageBox MB_OK "The setup app has been removed.$\r$\n$\r$\nRemote access is still running. To remove it completely, run the setup app again and choose Remove remote access before uninstalling."
SectionEnd
