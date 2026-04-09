//go:build !linux || (!amd64 && !arm64)

package seccomp

type Profile string

const (
	ProfileAPI  Profile = "api"
	ProfileVMM  Profile = "vmm"
	ProfileVCPU Profile = "vcpu"
)

func InstallWorkerProcessProfile() error {
	return nil
}

func InstallThreadProfile(Profile) error {
	return nil
}
