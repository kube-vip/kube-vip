package main

import "github.com/plunder-app/kube-vip/cmd"

// Version is populated from the Makefile and is tied to the release TAG
var Version string

// Build is the last GIT commit
var Build string

func main() {
	cmd.Release.Version = Version
	cmd.Release.Build = Build
	cmd.Execute()
}
