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

; Caddy is most of the download and compresses well. The default zlib produced
; a 21.8 MB installer, for a home user on a home connection.
SetCompressor /SOLID lzma

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

; A running RASA holds its own executable open, so installing over it fails
; with "Error opening file for writing: ...\rasa.exe" and the uninstaller
; silently leaves the file behind.
;
; The user cannot be expected to notice. Since 0.7 the wizard is built for the
; GUI subsystem so that double-clicking an installer does not leave a black
; console window on screen - which also means a copy left running has no
; window, no console and no taskbar entry. There is nothing to close and
; nothing to see. This installer also launches it on the way out, so the second
; run of the installer hits this every time.
;
; Closing it is exactly what RASA's own -replace flag does
; (internal/instance.Stop), and it is safe: setup state is persisted and
; resumable by design, and the lock file records an address rather than
; trusting a process id, so the next run takes over a lock left by a kill.
!macro CloseRunningRASA PREFIX
Function ${PREFIX}CloseRunningRASA
  Push $0
  Push $1

  ; Ask the question that actually matters -- can this file be written? --
  ; rather than the one that approximates it. A process name can be matched
  ; wrongly and a tasklist pipeline has to survive two layers of quoting
  ; through cmd; opening the file tests the exact condition that produced the
  ; error, and catches a lock held by something other than RASA too.
  ;
  ; Only when it already exists: on a first install there is nothing to open
  ; and no directory to open it in.
  IfFileExists "$INSTDIR\rasa.exe" 0 ${PREFIX}rasa_done

  ClearErrors
  ; Append mode: it opens for writing without truncating, so a file that is
  ; free is left exactly as it was found.
  FileOpen $0 "$INSTDIR\rasa.exe" a
  IfErrors 0 ${PREFIX}rasa_free
    MessageBox MB_OKCANCEL|MB_ICONINFORMATION \
      "RASA is already running.$\r$\n$\r$\nIt has no window of its own, so there is nothing on screen to close. Setup will close it and carry on.$\r$\n$\r$\nAnything it had already set up is saved, and running it again picks up where it left off." \
      IDOK ${PREFIX}rasa_close
    Abort "Setup stopped because RASA is still running."

    ${PREFIX}rasa_close:
    ; This needs the elevation the installer already holds. RASA relaunches
    ; itself as administrator at startup, so a copy left running cannot be
    ; closed from an ordinary command prompt -- which is how the original
    ; report ended with "Access is denied".
    ;
    ; No /T: that would also take the browser RASA opened, which belongs to
    ; the user rather than to setup.
    nsExec::ExecToStack 'taskkill /F /IM rasa.exe'
    Pop $0
    Pop $1
    ; Windows releases the handle a moment after the process goes, and writing
    ; too early fails exactly as if it had never been closed.
    Sleep 2000
    Goto ${PREFIX}rasa_done

  ${PREFIX}rasa_free:
  FileClose $0

  ${PREFIX}rasa_done:
  Pop $1
  Pop $0
FunctionEnd
!macroend

!insertmacro CloseRunningRASA ""
!insertmacro CloseRunningRASA "un."

Section "Install"
  Call CloseRunningRASA
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
  ; Same reason as the install side, with a quieter failure: Delete does not
  ; report an error, so a locked rasa.exe would leave the install half-removed
  ; and the uninstaller claiming success.
  Call un.CloseRunningRASA

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
