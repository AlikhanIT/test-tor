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

    Usage: torget [FLAGS] URL
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
    $ ./torget -name "haiku-beta1-x86.iso" "https://mirrors.rit.edu/haiku/r1beta5/haiku-r1beta5-x86_gcc2h-anyboot.iso"
    Output file: haiku-beta1-x86.iso
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