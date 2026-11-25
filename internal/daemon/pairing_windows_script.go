package daemon

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"strconv"
	"text/template"
)

// windowsScript is install.ps1: run in an administrator PowerShell on a
// Windows remote (the command Add Remote shows downloads it with
// curl.exe, which checks the controller's key pin, then runs it). It puts
// the daemon in Program Files\charm-bak with restic.exe beside it (a
// service runs as SYSTEM, without the PATH of whoever installed restic),
// keeps state in ProgramData\charm-bak for SYSTEM and administrators
// only, pairs, and runs the daemon as the service charm-bak listening on
// the remote port, restarted on failure (which is also how a self-update
// takes effect), with a firewall rule for that port.
var windowsScript = template.Must(template.New("install.ps1").Parse(`# charm-bak: {{if .Repair}}reinstall this machine's backup daemon (controlled by {{.Controller}}), keeping its state.{{else}}install this machine's backup daemon and pair it with {{.Controller}}.{{end}}
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$me = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $me.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  Write-Host 'charm-bak: run this in an administrator PowerShell (right-click Start, then Terminal (Admin)).' -ForegroundColor Red
  exit 1
}
switch ($env:PROCESSOR_ARCHITECTURE) {
  'AMD64' { $arch = 'amd64' }
  'ARM64' { $arch = 'arm64' }
  default { Write-Host "charm-bak: no daemon for $env:PROCESSOR_ARCHITECTURE" -ForegroundColor Red; exit 1 }
}
$dir = Join-Path $env:ProgramFiles 'charm-bak'
$state = Join-Path $env:ProgramData 'charm-bak'
$exe = Join-Path $dir 'charm-bak-daemon.exe'
if (Get-Service charm-bak -ErrorAction SilentlyContinue) { Stop-Service charm-bak -Force -ErrorAction SilentlyContinue }
New-Item -ItemType Directory -Force $dir, $state | Out-Null
# State for SYSTEM and administrators only (by SID, so any language).
# Anyone can create a folder in ProgramData, so one made here first by
# another user would keep that user's own entries and, as its owner, the
# right to change them: Administrators take it over, every existing entry
# goes (/reset), then only the two grants remain.
icacls $state /setowner '*S-1-5-32-544' /T /C | Out-Null
icacls $state /reset /T /C | Out-Null
icacls $state /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
if ($LASTEXITCODE -ne 0) { Write-Host "charm-bak: couldn't restrict $state to administrators" -ForegroundColor Red; exit 1 }

curl.exe -fsS --pinnedpubkey '{{.Pin}}' -k "https://{{.Addr}}/x/{{.Code}}/$arch" -o "$exe.new"
if ($LASTEXITCODE -ne 0) { Write-Host "charm-bak: couldn't download the daemon from {{.Controller}}" -ForegroundColor Red; exit 1 }
Move-Item -Force "$exe.new" $exe
Remove-Item "$exe.old" -ErrorAction SilentlyContinue

$restic = Join-Path $dir 'restic.exe'
if (-not (Test-Path $restic)) {
  $found = Get-Command restic -ErrorAction SilentlyContinue | Select-Object -First 1 -ExpandProperty Source
  if (-not $found -and (Get-Command winget -ErrorAction SilentlyContinue)) {
    Write-Host 'charm-bak: installing restic with winget...'
    winget install --id restic.restic -e --accept-package-agreements --accept-source-agreements --disable-interactivity | Out-Null
    $found = Get-ChildItem (Join-Path $env:LOCALAPPDATA 'Microsoft\WinGet\Packages') -Recurse -Filter 'restic*.exe' -ErrorAction SilentlyContinue | Select-Object -First 1 -ExpandProperty FullName
  }
  if ($found) { Copy-Item $found $restic -Force }
  else { Write-Host "charm-bak: restic isn't installed; backups need it. Install it (winget install restic.restic) and copy restic.exe to $dir" -ForegroundColor Yellow }
}
{{if not .Repair}}
& $exe -name {{.Name}} -pair 'https://{{.Addr}}/p/{{.Code}}' -controller-pin '{{.Pin}}'
if ($LASTEXITCODE -ne 0) { Write-Host 'charm-bak: pairing failed' -ForegroundColor Red; exit 1 }
{{end}}
if (-not (Get-Service charm-bak -ErrorAction SilentlyContinue)) {
  New-Service -Name charm-bak -BinaryPathName ('"' + $exe + '" -name {{.Name}} -listen :{{.RemotePort}}') -StartupType Automatic -DisplayName 'charm-bak backup daemon' -Description 'Backs up this machine (controlled by {{.Controller}})' | Out-Null
}
sc.exe failure charm-bak reset= 86400 actions= restart/5000/restart/5000/restart/5000 | Out-Null
sc.exe failureflag charm-bak 1 | Out-Null
netsh advfirewall firewall delete rule name=charm-bak | Out-Null
netsh advfirewall firewall add rule name=charm-bak dir=in action=allow protocol=TCP localport={{.RemotePort}} program="$exe" | Out-Null
Start-Service charm-bak
Write-Host 'charm-bak: {{.Name}} is {{if .Repair}}reinstalled{{else}}paired with {{.Controller}}{{end}} and running.' -ForegroundColor Green
`))

// pairWindowsScript serves install.ps1.
func (c *Core) pairWindowsScript(addr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, repair, ok := c.validCodeFor(r.PathValue("code"))
		if !ok {
			http.Error(w, "Write-Host 'charm-bak: that code has expired; start again in charm-bak.' -ForegroundColor Red; exit 1", http.StatusNotFound)
			return
		}
		var b bytes.Buffer
		windowsScript.Execute(&b, map[string]any{
			"Controller": c.Name, "Name": name, "Pin": c.identity.Pin, "Code": r.PathValue("code"),
			"Addr": net.JoinHostPort(addr, strconv.Itoa(c.pairPort())), "RemotePort": RemotePort, "Repair": repair,
		})
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(b.Bytes())
	}
}

// pairWindowsBinary serves the Windows daemon for an architecture.
func (c *Core) pairWindowsBinary(w http.ResponseWriter, r *http.Request) {
	if _, ok := c.validCode(r.PathValue("code")); !ok {
		http.Error(w, "expired", http.StatusNotFound)
		return
	}
	path := c.remoteDaemon("windows", r.PathValue("arch"))
	if path == "" {
		http.Error(w, "no such build", http.StatusNotFound)
		return
	}
	if _, err := os.Stat(path); err != nil {
		http.Error(w, "this app has no windows-"+r.PathValue("arch")+" daemon", http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, path)
}

// windowsCommand is what to paste in an administrator PowerShell on a
// Windows remote: download install.ps1 with the controller's key pinned,
// then run it.
func windowsCommand(pin, hostport, code string) string {
	return "curl.exe -fsS --pinnedpubkey '" + pin + "' -k https://" + hostport + "/w/" + code +
		" -o $env:TEMP\\charm-bak.ps1; powershell -ExecutionPolicy Bypass -File $env:TEMP\\charm-bak.ps1"
}
