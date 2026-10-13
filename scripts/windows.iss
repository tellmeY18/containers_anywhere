; Per-user launcher. Uninstall never removes cluster data or downloaded runtimes.
[Setup]
AppId=org.containers-anywhere.launcher
AppName=Containers Anywhere
AppVersion={#AppVersion}
DefaultDirName={localappdata}\Programs\Containers Anywhere
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
OutputDir={#OutputDir}
OutputBaseFilename=Containers-Anywhere-{#AppVersion}-windows-amd64
Compression=lzma2/fast
SolidCompression=yes
WizardStyle=modern
UninstallDisplayIcon={app}\containers-anywhere.exe
CloseApplications=yes
RestartApplications=no
SetupLogging=yes

[Files]
Source: "{#AppSource}\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs

[Icons]
Name: "{userprograms}\Containers Anywhere"; Filename: "{app}\containers-anywhere.exe"
Name: "{userdesktop}\Containers Anywhere"; Filename: "{app}\containers-anywhere.exe"

[Run]
Filename: "{app}\containers-anywhere.exe"; Description: "Open Containers Anywhere"; Flags: nowait postinstall skipifsilent
