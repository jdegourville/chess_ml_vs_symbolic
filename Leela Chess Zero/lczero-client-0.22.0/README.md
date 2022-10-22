# Compiling

You will need to install Go 1.9 or later.

Then, make sure to set up your GOPATH properly, eg. here is mine:
```
export GOPATH=${HOME}/go:${HOME}/src/lczero-client
```
Here, I've set my system install of go as the first entry, and then the lczero-client directory as the second.

Pre-reqs:
```
# (Bug workaround, using Tilps instead)
# go get -u github.com/notnil/chess
go get -u github.com/Tilps/chess
go get -u github.com/nightlyone/lockfile

```

Pull or download the `master` branch

Then to produce a `lczero-client` executable:
`go build lc0_main.go` for the `lc0` client

# Running

First copy the `lc0` executable into the same folder as the `lczero-client` executable.

Then, run!  Username and password are required parameters.
```
./lczero-client --user=myusername --password=mypassword
```

For testing, you can also point the client at a different server:
```
./lczero-client --hostname=http://127.0.0.1:8080 --user=test --password=asdf
```

# Cross-compiling

One of the main reasons I picked go was it's amazing support for cross-compiling.

Pre-reqs:
```
GOOS=windows GOARCH=amd64 go install
GOOS=darwin GOARCH=amd64 go install
GOOS=linux GOARCH=amd64 go install
```

Building the client for each platform:
```
GOOS=windows GOARCH=amd64 go build -o lczero-client.exe
GOOS=darwin GOARCH=amd64 go build -o lczero-client_mac
GOOS=linux GOARCH=amd64 go build -o lczero-client_linux
```
