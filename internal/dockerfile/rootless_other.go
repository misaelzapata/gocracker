//go:build !linux

package dockerfile

import "fmt"

func (b *builder) runRootless(args, envArgs []string) error {
	return fmt.Errorf("rootless RUN is only available on Linux")
}

func (b *builder) runPrivileged(args, envArgs []string, mounts []RunMount) error {
	return fmt.Errorf("privileged RUN isolation is only available on Linux")
}

func rootlessErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
