$OSes = @("windows", "linux", "freebsd", "openbsd", "netbsd", "darwin")
$Env:GOARCH = "amd64"

New-Item -Path "./dist/" -ItemType Directory

$OSes | ForEach-Object {
    Write-Host "Building for $_..." -NoNewline
    $Env:GOOS = $_
    go build -o "./dist/torget-$_" -ldflags "-w -s" ".\torget.go"
    Write-Host " Done" -ForegroundColor Green
}

Move-Item -Path "./dist/torget-windows" -Destination "./dist/torget.exe"