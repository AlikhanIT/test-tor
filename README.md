# torget

## Description

The tool downloads large files over a locally installed Tor client by
aggressively discovering a pool of fast circuits and using them in parallel.
With slow servers, this strategy bypasses per-IP traffic shaping, resulting in
much faster downloads.

Onion services are fully supported.

## Building From Source

    $ git clone https://github.com/mtrojnar/torget.git
    [...]
    $ cd torget
    $ go build torget.go

## Using

### View help page
    $ ./torget -h
    torget 2.0, a fast large file downloader over locally installed Tor
    Copyright © 2021-2023 Michał Trojnara <Michal.Trojnara@stunnel.org>
    Licensed under GNU GPL version 3 <https://www.gnu.org/licenses/>

    Usage: torget [FLAGS] {file.txt | URL [URL2...]}
      -circuits, -c int
            Concurrent circuits. (default 20)
      -destination, -d string
            Output directory. (default current directory)
      -force bool
            Will create parent folder(s) and/or overwrite existing files.
      -min-lifetime, -l int
            Minimum circuit lifetime (seconds). (default 10)
      -name, -n string
            Output filename. (default filename from URL)
      -tor-port, -p int
            Port your Tor service is listening on. (default 9050)
      -verbose, -v
            Show diagnostic details.

### Download a file to the current directory
    $ ./torget "https://archive.org/download/grimaces-birthday-game/Grimace%27s%20Birthday%20%28World%29%20%28v1.7%29%20%28Aftermarket%29%20%28Unl%29.gbc"
    Output file: Grimace's Birthday (World) (v1.7) (Aftermarket) (Unl).gbc
    Download length:   1.00 MiB
    Download complete - 20s

### Specify a destination directory
    $ ./torget -destination "/isos" "https://cdimage.ubuntu.com/kubuntu/releases/24.10/release/kubuntu-24.10-desktop-amd64.iso"
    Output file: /isos/kubuntu-24.10-desktop-amd64.isoDownload length:   4.36 GiB
    Download complete - 5m43s

### Specify a new name for the file
    $ ./torget -name "haiku-beta5-x86.iso" "https://mirrors.rit.edu/haiku/r1beta5/haiku-r1beta5-x86_gcc2h-anyboot.iso"
    Output file: haiku-beta5-x86.iso
    Download length:   1.38 GiB
    Download complete - 1m47s

### The -force flag
Use the -force flag to create parent directories and/or overwrite existing files

    $ ./torget -force -destination "/movies/Big Buck Bunny - Sunflower version (2013)" -name "Big Buck Bunny - Sunflower Version (2013).mp4" "http://distribution.bbb3d.renderfarming.net/video/mp4/bbb_sunflower_native_60fps_normal.mp4"
    Output file: /movies/Big Buck Bunny - Sunflower version (2013)/Big Buck Bunny - Sunflower Version (2013).mp4
    Download length: 793.33 MiB
    Download complete - 2m52s

If the destination directory is not found, the program will try downloading to the current directory instead.

    WARNING: Unable to find destination "/movies/Big Buck Bunny - Sunflower version (2013)".
    Trying current directory instead.

If the file already exists, you will receive an error.

    ERROR: "/movies/Big Buck Bunny - Sunflower version (2013)/Big Buck Bunny - Sunflower Version (2013).mp4" already exists.
    exit status 1

## Downloading Multiple Files

### With multiple URLs as arguments
You may pass more than one URL to torget and download them all sequentially.

    $ ./torget -destination "/music" "https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD1.zip" "https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD2v.zip" "https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD3.zip"
    Downloading 3 files.

    [1/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD1.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD1.zip
    Download length: 169.00 MiB
    Download complete - 1m16s
    
    [2/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD2v.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD2v.zip
    Download length: 135.64 MiB
    Download complete - 1m5s
    
    [3/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD3.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD3.zip
    Download length: 145.64 MiB
    Download complete - 1m15s

### With a local text file
You may pass the path of a text file containing multiple URLs on each line to download them all sequentially.

#### /music/downloads.txt
    
    https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD4.zip
    https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD5.zip
    https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD6.zip

#### Run torget

    $ torget -destination "/music" "/music/downloads.txt"
    Downloading 3 files.

    [1/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD4.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD4.zip
    Download length: 155.83 MiB
    Download complete - 1m13s
    
    [2/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD5.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD5.zip
    Download length: 152.86 MiB
    Download complete - 1m4s
    
    [3/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD6.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD6.zip
    Download length: 159.02 MiB
    Download complete - 1m9s
