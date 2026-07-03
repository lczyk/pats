# linux environment for the bwrap e2e tests (src/sandbox/bwrap_linux_test.go)
# on non-linux dev machines -- see `make test-bwrap`. go pinned to match go.mod.
FROM golang:1.26
RUN apt-get update -qq && apt-get install -y -qq bubblewrap curl
