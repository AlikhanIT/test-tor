# tor-dl

tor-dl is a tool to download large files over a locally installed Tor client
by aggressively discovering a pool of fast circuits and using them in
parallel. With slow servers, this strategy bypasses per-IP traffic shaping,
resulting in much faster downloads. Onion services are fully supported.

tor-dl is based on the work-in-progress 2.0 version of
[torget by Michał Trojnara](https://github.com/mtrojnar/torget). The two most
impactful updates are:

- Specify download destination folders and/or filenames
- Download multiple files

## Building From Source

    $ git clone https://github.com/BryanCuneo/tor-dl.git
    [...]
    $ cd tor-dl
    $ go build tor-dl.go

## Using

### View help page
    $ ./tor-dl -h
    tor-dl - fast large file downloader over locally installed Tor
    Copyright © 2025 Bryan Cuneo <https://github.com/BryanCuneo/tor-dl/>
    Licensed under GNU GPL version 3 <https://www.gnu.org/licenses/>
    Based on torget by Michał Trojnara <https://github.com/mtrojnar/torget>

    Usage: tor-dl [FLAGS] {file.txt | URL [URL2...]}
      -allow-http bool
            Allow tor-dl to download files over HTTP instead of HTTPS. Not recommended!
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
      -quiet, -q bool
            Suppress most text output (still show errors).
      -silent, -s bool
            Suppress all text output (including errors).
      -tor-port, -p int
            Port your Tor service is listening on. (default 9050)
      -verbose, -v
            Show diagnostic details.

### Download a file to the current directory
    $ ./tor-dl "https://archive.org/download/grimaces-birthday-game/Grimace%27s%20Birthday%20%28World%29%20%28v1.7%29%20%28Aftermarket%29%20%28Unl%29.gbc"
    Output file: Grimace's Birthday (World) (v1.7) (Aftermarket) (Unl).gbc
    Download length:   1.00 MiB
    Download completed in 20s

### Specify a destination directory
    $ ./tor-dl -destination "/isos" "https://cdimage.ubuntu.com/kubuntu/releases/24.10/release/kubuntu-24.10-desktop-amd64.iso"
    Output file: /isos/kubuntu-24.10-desktop-amd64.isoDownload length:   4.36 GiB
    Download completed in 5m43s

### Specify a new name for the file
    $ ./tor-dl -name "haiku-beta5-x86.iso" "https://mirrors.rit.edu/haiku/r1beta5/haiku-r1beta5-x86_gcc2h-anyboot.iso"
    Output file: haiku-beta5-x86.iso
    Download length:   1.38 GiB
    Download completed in 1m47s

### The -force flag
Use the -force flag to create parent directories and/or overwrite existing files

    $ ./tor-dl -force -destination "/movies/Big Buck Bunny - Sunflower version (2013)" -name "Big Buck Bunny - Sunflower Version (2013).mp4" "http://distribution.bbb3d.renderfarming.net/video/mp4/bbb_sunflower_native_60fps_normal.mp4"
    Output file: /movies/Big Buck Bunny - Sunflower version (2013)/Big Buck Bunny - Sunflower Version (2013).mp4
    Download length: 793.33 MiB
    Download completed in 2m52s

If the destination directory is not found, the program will try downloading to the current directory instead.

    WARNING: Unable to find destination "/movies/Big Buck Bunny - Sunflower version (2013)".
    Trying current directory instead.

If the file already exists, you will receive an error.

    ERROR: "/movies/Big Buck Bunny - Sunflower version (2013)/Big Buck Bunny - Sunflower Version (2013).mp4" already exists.
    exit status 1

## Downloading Multiple Files

### With multiple URLs as arguments
You may pass more than one URL to tor-dl and download them all sequentially.

    $ ./tor-dl -destination "/music" "https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD1.zip" "https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD2v.zip" "https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD3.zip"
    Downloading 3 files.

    [1/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD1.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD1.zip
    Download length: 169.00 MiB
    Download completed in 1m16s
    
    [2/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD2v.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD2v.zip
    Download length: 135.64 MiB
    Download completed in 1m5s
    
    [3/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD3.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD3.zip
    Download length: 145.64 MiB
    Download completed in 1m15s

### With a local text file
You may pass the path of a text file containing multiple URLs on each line to download them all sequentially.

#### /music/downloads.txt
    
    https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD4.zip
    https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD5.zip
    https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD6.zip

#### Run tor-dl

    $ tor-dl -destination "/music" "/music/downloads.txt"
    Downloading 3 files.

    [1/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD4.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD4.zip
    Download length: 155.83 MiB
    Download completed in 1m13s
    
    [2/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD5.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD5.zip
    Download length: 152.86 MiB
    Download completed in 1m4s
    
    [3/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD6.zip
    Output file: /music/ca200_Various__Clinical_Jazz_CD6.zip
    Download length: 159.02 MiB
    Download completed in 1m9s

# A Note on Privacy
Neither this tool nor Tor in general guarantees anonymity. Please review
[this blog post](https://support.torproject.org/faq/staying-anonymous/) from
the Tor Project for tips on protectecting yourself. Especially relevant to
tor-dl is the section titled, "Don't open documents downloaded through Tor
while online." tor-dl is primarily designed for the use case of evading
network restrictions by routing your traffic through Tor. But it will have no
impact on how you use the files after they've been downoloaded.

## Downloads over HTTP
By default, tor-dl will not allow you to download files over HTTP:

    $ ./tor-dl http://archive.org/download/dracula_librivox/DraculaStoker_64kb_librivox.m4b
    ERROR: "http://archive.org/download/dracula_librivox/DraculaStoker_64kb_librivox.m4b" is not using HTTPS.
        If you absolutely must use HTTP, use the -allow-http flag. This is dangerous and not recommended!

If you understand the privacy concerns and still want to download over HTTP
anyway, you can use the `-allow-http` flag:

    $ ./tor-dl -allow-http http://archive.org/download/dracula_librivox/DraculaStoker_64kb_librivox.m4b
    Output file: DraculaStoker_64kb_librivox.m4b
    Download length: 594.70 MiB
    Download completed in 3m44s