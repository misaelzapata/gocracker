package vmm

import "testing"

func TestResolveWorkerHostSidePath(t *testing.T) {
	cases := []struct {
		name      string
		meta      WorkerMetadata
		guestPath string
		want      string
	}{
		{
			name:      "jailer off passes through",
			meta:      WorkerMetadata{},
			guestPath: "/var/run/gocracker/vm.sock",
			want:      "/var/run/gocracker/vm.sock",
		},
		{
			name:      "empty guest path",
			meta:      WorkerMetadata{JailRoot: "/srv/jailer/x/root", RunDir: "/tmp/wkr"},
			guestPath: "",
			want:      "",
		},
		{
			name:      "jailer on, worker bind prefix",
			meta:      WorkerMetadata{JailRoot: "/srv/jailer/abc/root", RunDir: "/tmp/worker-123"},
			guestPath: "/worker/vm.sock",
			want:      "/tmp/worker-123/vm.sock",
		},
		{
			name:      "jailer on, /worker root itself",
			meta:      WorkerMetadata{JailRoot: "/srv/jailer/abc/root", RunDir: "/tmp/worker-123"},
			guestPath: "/worker",
			want:      "/tmp/worker-123",
		},
		{
			name:      "jailer on, non-bind path falls back to jail root",
			meta:      WorkerMetadata{JailRoot: "/srv/jailer/abc/root", RunDir: "/tmp/worker-123"},
			guestPath: "/run/gocracker/vm.sock",
			want:      "/srv/jailer/abc/root/run/gocracker/vm.sock",
		},
		{
			name:      "jailer on but RunDir unknown, /worker falls back to jailRoot",
			meta:      WorkerMetadata{JailRoot: "/srv/jailer/abc/root"},
			guestPath: "/worker/vm.sock",
			want:      "/srv/jailer/abc/root/worker/vm.sock",
		},
		{
			name:      "worker-like prefix but not exactly /worker/",
			meta:      WorkerMetadata{JailRoot: "/srv/jailer/abc/root", RunDir: "/tmp/wkr"},
			guestPath: "/workerx/vm.sock",
			want:      "/srv/jailer/abc/root/workerx/vm.sock",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveWorkerHostSidePath(tc.meta, tc.guestPath)
			if got != tc.want {
				t.Fatalf("ResolveWorkerHostSidePath(%+v, %q) = %q, want %q",
					tc.meta, tc.guestPath, got, tc.want)
			}
		})
	}
}

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
