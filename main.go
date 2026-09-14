package main

import (
	"github.com/giantswarm/vm-manager/cmd"
	"github.com/giantswarm/vm-manager/pkg/project"
)

func main() {
	cmd.SetVersion(project.Version())
	cmd.SetBuildInfo(project.GitSHA(), project.BuildTimestamp())
	cmd.Execute()
}
