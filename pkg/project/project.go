// Package project exposes the build metadata of the vm-manager binary. The
// three values are stamped with -ldflags -X by the generated Makefile
// (Makefile.gen.go.mk) and by the CircleCI go-build job, which sets gitSHA and
// buildTimestamp; a plain `go build` reports the defaults.
package project

var (
	version        = "dev"
	gitSHA         = "unknown"
	buildTimestamp = "unknown"
)

// Version is the release version (the git tag without "v"), "dev" otherwise.
func Version() string { return version }

// GitSHA is the commit the binary was built from.
func GitSHA() string { return gitSHA }

// BuildTimestamp is the UTC RFC 3339 time of the build.
func BuildTimestamp() string { return buildTimestamp }
