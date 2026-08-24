; NSIS installer for RASA for Jellyfin.
;
; Unsigned by design (decision 13): there is no code-signing budget, so
; SmartScreen will warn on every download. That is documented on the Pages
; site rather than worked around.

!define APPNAME "RASA for Jellyfin"
!define VERSION "1.0.0"

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

  ; Firewall rules are scoped to the Caddy binary rather than to a port,
  ; because the listener port is not known until setup runs - and a
  ; program-scoped rule is tighter than opening ports outright.
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="RASA for Jellyfin"'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="RASA for Jellyfin" dir=in action=allow program="$INSTDIR\caddy.exe" enable=yes profile=any'

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
  Delete "$INSTDIR\rasa.exe"
  Delete "$INSTDIR\uninstall.exe"
  Delete "$SMPROGRAMS\${APPNAME}.lnk"
  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RASA"

  MessageBox MB_OK "The setup app has been removed.$\r$\n$\r$\nRemote access is still running. To remove it completely, run the setup app again and choose Remove remote access before uninstalling."
SectionEnd
