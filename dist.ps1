$OSes = @("windows", "linux", "freebsd", "openbsd", "netbsd", "darwin")
$Env:GOARCH = "amd64"
$executable = "./dist/tor-dl"
$win_executable = "./dist/tor-dl.exe"
$license = "./LICENSE"
$readme = "./README.md"

New-Item -Path "./dist/" -ItemType Directory -ErrorAction SilentlyContinue | Out-Null

$OSes | ForEach-Object {
    Write-Host "Building for $_..." -NoNewline
    $Env:GOOS = $_

    if ($_ -like "windows") {
        $out = $win_executable
    }
    else {
        $out = $executable
    }

    go build -o $out -ldflags "-w -s" ".\tor-dl.go"

    # Build zip archive with executable, license file, and readme file
    $zip = "./dist/tor-dl-$_-$Env:GOARCH.zip"
    Compress-Archive -Force -Path $out -DestinationPath $zip
    Compress-Archive -Update -Path $license -DestinationPath $zip
    Compress-Archive -Update -Path $readme -DestinationPath $zip 

    Remove-Item -Path $out

    Write-Host " Done" -ForegroundColor Green
}