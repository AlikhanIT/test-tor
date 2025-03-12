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

## Usage

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
    Output file:            Grimace's Birthday (World) (v1.7) (Aftermarket) (Unl).gbc
    Download filesize:      1.00 MiB
    Download completed in:  16s (64.03 KiB/s)

### Specify a destination directory
    $ ./tor-dl -destination "/isos/" "https://download.tails.net/tails/stable/tails-amd64-6.13/tails-amd64-6.13.img"
    Output file:            /isos/tails-amd64-6.13.img
    Download filesize:      1.48 GiB
    Download completed in:  2m28s (7.30 MiB/s)

### Specify a new name for the file
    $ ./tor-dl -name "haiku-beta5-32bit.iso" "https://mirrors.rit.edu/haiku/r1beta5/haiku-r1beta5-x86_gcc2h-anyboot.iso"
    Output file:            haiku-beta5-32bit.iso
    Download filesize:      1.38 GiB
    Download completed in:  3m46s (6.23 MiB/s)

### The -force flag
By default, tor-dl will not create any parent directories or overwrite
existing files. If the file already exists in your destination, then you
will see the following error:

    $ tor-dl -destination "/audiobooks/" "https://archive.org/download/dracula_librivox/DraculaStoker_64kb_librivox.m4b"
    ERROR: "/audiobooks/DraculaStoker_64kb_librivox.m4b" already exists. Skipping.

If instead a directory in your `-destination` path doesn't exist, you
you will get a warning and the download will default to your current
directory instead:

    $ tor-dl -destination "/audiobooks/" "https://archive.org/download/dracula_librivox/DraculaStoker_64kb_librivox.m4b"
    WARNING: Unable to find destination "/audiobooks/". Trying current directory instead.
    Output file:            DraculaStoker_64kb_librivox.m4b
    Download filesize:      594.70 MiB
    Download completed in:  3m21s (2.97 MiB/s)

To automatically create new drectories and/or overwrite existing files, you
can use the `-force` flag:

    $ tor-dl -force -destination "/audiobooks/" "https://archive.org/download/dracula_librivox/DraculaStoker_64kb_librivox.m4b"
    Output file:            /audiobooks/DraculaStoker_64kb_librivox.m4b
    Download filesize:      594.70 MiB
    Download completed in:  3m21s (2.97 MiB/s)

## Downloading Multiple Files

### With multiple URLs as arguments
You may pass more than one URL to tor-dl and download them all sequentially.

    $ ./tor-dl -destination "/music" "https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD1.zip" "https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD2v.zip" "https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD3.zip"
    Downloading 3 files.

    [1/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD1.zip
    Output file:            /music/ca200_Various__Clinical_Jazz_CD1.zip
    Download filesize:      169.00 MiB
    Download completed in:  55s (3.05 MiB/s) 
    
    [2/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD2v.zip
    Output file:            /music/ca200_Various__Clinical_Jazz_CD2v.zip
    Download filesize:      135.64 MiB
    Download completed in:  1m2s (2.19 MiB/s)
    
    [3/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD3.zip
    Output file:            /music/ca200_Various__Clinical_Jazz_CD3.zip
    Download filesize:      145.64 MiB
    Download completed in:  56s (2.62 MiB/s)

### With a local text file
You may pass the path of a text file containing multiple URLs on each line to
download them all sequentially.

#### /music/downloads.txt
    
    https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD4.zip
    https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD5.zip
    https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD6.zip

#### Run tor-dl

    $ tor-dl -destination "/music" "/music/downloads.txt"
    Downloading 3 files.

    [1/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD4.zip
    Output file:            /music/ca200_Various__Clinical_Jazz_CD4.zip
    Download filesize:      155.83 MiB
    Download completed in:  46s (3.41 MiB/s)
    
    [2/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD5.zip
    Output file:            /music/ca200_Various__Clinical_Jazz_CD5.zip
    Download filesize:      152.86 MiB
    Download completed in:  39s (3.93 MiB/s)
    
    [3/3] - https://archive.org/download/ca200_cjazz/ca200_Various__Clinical_Jazz_CD6.zip
    Output file:            /music/ca200_Various__Clinical_Jazz_CD6.zip
    Download filesize:      159.02 MiB
    Download completed in:  55s (2.87 MiB/s)

# A Note on Privacy
Neither this tool nor Tor in general guarantees anonymity. Please review
[this blog post](https://support.torproject.org/faq/staying-anonymous/) from
the Tor Project for tips on protectecting yourself. Especially relevant to
tor-dl is the section titled, "Don't open documents downloaded through Tor
while online." tor-dl is primarily designed for the use case of evading
network restrictions by routing your traffic through Tor and it will have no
impact on how you use the files after they've been downoloaded.

## Downloads over HTTP
By default, tor-dl will not allow you to download files over HTTP:

    $ ./tor-dl "http://distribution.bbb3d.renderfarming.net/video/mp4/bbb_sunflower_native_60fps_normal.mp4"
    ERROR: "http://distribution.bbb3d.renderfarming.net/video/mp4/bbb_sunflower_native_60fps_normal.mp4" is not using HTTPS.
    	If you absolutely must use HTTP, use the -allow-http flag. This is dangerous and not recommended!

If you understand the privacy concerns and still want to download over HTTP
anyway, you can use the `-allow-http` flag:

    $ ./tor-dl -allow-http "http://distribution.bbb3d.renderfarming.net/video/mp4/bbb_sunflower_native_60fps_normal.mp4"
    Output file:            bbb_sunflower_native_60fps_normal.mp4
    Download filesize:      793.33 MiB
    Download completed in:  2m57s (4.47 MiB/s)
