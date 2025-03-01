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
    Licensed under GNU/GPL version 3

    Usage: torget [FLAGS] URL [URL2...]
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
    -verbose, -v
          Show diagnostic details.

### Download a file to the current directory
    $ ./torget "https://archive.org/download/grimaces-birthday-game/Grimace%27s%20Birthday%20%28World%29%20%28v1.7%29%20%28Aftermarket%29%20%28Unl%29.gbc"
    Output file: Grimace's Birthday (World) (v1.7) (Aftermarket) (Unl).gbc
    Download length: 1048576 bytes
    Download complete

### Specify a destination directory
    $ ./torget -destination "/isos" "https://cdimage.ubuntu.com/kubuntu/releases/24.10/release/kubuntu-24.10-desktop-amd64.iso"
    Output file: /isos/kubuntu-24.10-desktop-amd64.iso
    Download length: 4678961152 bytes
    Download complete

### Specify a new name for the file
    $ ./torget -name "haiku-beta5-x86.iso" "https://mirrors.rit.edu/haiku/r1beta5/haiku-r1beta5-x86_gcc2h-anyboot.iso"
    Output file: haiku-beta5-x86.iso
    Download length: 1477246976 bytes
    Download complete

### The -force flag
Use the -force flag to create parent directories and/or overwrite existing files

    $ ./torget -force -destination "/movies/Big Buck Bunny - Sunflower version (2013)" -name "Big Buck Bunny - Sunflower Version (2013).mp4" "http://distribution.bbb3d.renderfarming.net/video/mp4/bbb_sunflower_native_60fps_normal.mp4"
    Output file: /movies/Big Buck Bunny - Sunflower version (2013)/Big Buck Bunny - Sunflower Version (2013).mp4
    Download length: 831871244 bytes
    Download complete

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
    Download length: 177206596 bytes
    Download complete
    
    [2/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD2v.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD2v.zip
    Download length: 142228026 bytes
    Download complete
    
    [3/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD3.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD3.zip
    Download length: 152713364 bytes
    Download complete

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
    Download length: 163400499 bytes
    Download complete
    
    [2/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD5.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD5.zip
    Download length: 160286763 bytes
    Download complete
    
    [3/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD6.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD6.zip
    Download length: 166744974 bytes
    Download complete