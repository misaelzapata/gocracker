package vmm

import "testing"

func TestResolveHostSidePath(t *testing.T) {
	cases := []struct {
		name      string
		jailRoot  string
		guestPath string
		want      string
	}{
		{"jailer off", "", "/var/run/gocracker/sandboxes/vm-1.sock", "/var/run/gocracker/sandboxes/vm-1.sock"},
		{"jailer on", "/srv/jailer/gocracker-vmm/abc/root", "/run/gocracker/sandboxes/vm-1.sock", "/srv/jailer/gocracker-vmm/abc/root/run/gocracker/sandboxes/vm-1.sock"},
		{"empty guest path", "/srv/jailer/gocracker-vmm/abc/root", "", ""},
		{"empty guest path, no jail", "", "", ""},
		{"guest path with dashes and uuids", "/srv/jailer/gocracker-vmm/jail-xyz/root", "/run/gocracker/sandboxes/a1b2c3d4-e5f6.sock", "/srv/jailer/gocracker-vmm/jail-xyz/root/run/gocracker/sandboxes/a1b2c3d4-e5f6.sock"},
		{"jail root with trailing slash", "/srv/jailer/gocracker-vmm/abc/root/", "/run/gocracker/vm.sock", "/srv/jailer/gocracker-vmm/abc/root/run/gocracker/vm.sock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveHostSidePath(tc.jailRoot, tc.guestPath)
			if got != tc.want {
				t.Fatalf("ResolveHostSidePath(%q, %q) = %q, want %q", tc.jailRoot, tc.guestPath, got, tc.want)
			}
		})
	}
}
