# github-mirror
Local cache github releases files for fast download.

## Install
```bash
go get -v github.com/codeskyblue/github-mirror
```

## Usage
```bash
# listen on port 8000, store cached data in dir:data
$ github-mirror -p 8000 -d data

# use proxy eg http://127.0.0.1:1080
$ github-mirror -proxy http://127.0.0.1:1080

# use proxy get from command, every request will call command again
$ github-mirror -proxy "echo http://127.0.0.1:1080"
```

When you want to download file <https://github.com/openatx/atx-agent/releases/download/0.3.5/atx-agent_0.3.5_checksums.txt>
but the it is very slow.

Change download url to <http://localhost:8000/openatx/atx-agent/releases/download/0.3.5/atx-agent_0.3.5_checksums.txt>

> PS: change http://localhost:8000 is your github-mirror listen on other address

If multi people request one resources, only one download thread will be created.
And when downloaded, every download request will be satisfied.

# LICENSE
[MIT](LICENSE)