//go:build linux

package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"unsafe"
)

const (
	kvmDevice = "/dev/kvm"

	// ioctlKVMGetAPIVersion is KVM_GET_API_VERSION. The KVM API documentation states
	// that applications must refuse to run unless it returns exactly 12.
	ioctlKVMGetAPIVersion = 0xAE00
	expectedKVMAPIVersion = 12
)

// virtualizationCheck verifies that a usable KVM device is present. Opening the device and
// issuing the version ioctl is the only reliable test: the file can exist while the
// hypervisor is unusable, and /proc/cpuinfo flags say nothing about nested virtualisation
// actually being exposed to this guest.
func virtualizationCheck() Check {
	return Check{
		Name: "virtualization",
		Run: func(ctx context.Context, env Env) Result {
			fd, err := syscall.Open(kvmDevice, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
			if err != nil {
				return kvmOpenFailure(err)
			}
			defer syscall.Close(fd)

			version, _, errno := syscall.RawSyscall(
				syscall.SYS_IOCTL, uintptr(fd), ioctlKVMGetAPIVersion, uintptr(unsafe.Pointer(nil)))
			if errno != 0 {
				return Result{
					Status: StatusFail,
					Detail: kvmDevice + " present, ioctl failed",
					Remedy: fmt.Sprintf("KVM_GET_API_VERSION on %s failed: %v.\n"+
						"The device exists but is not usable by this process.", kvmDevice, errno),
				}
			}
			if version != expectedKVMAPIVersion {
				return Result{
					Status: StatusFail,
					Detail: fmt.Sprintf("KVM API version %d", version),
					Remedy: fmt.Sprintf("Boks expects KVM API version %d, got %d. "+
						"Refusing to use an unrecognised KVM interface.", expectedKVMAPIVersion, version),
				}
			}
			return Result{Status: StatusOK, Detail: "KVM available (" + kvmDevice + ")"}
		},
	}
}

func kvmOpenFailure(err error) Result {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Result{
			Status: StatusFail,
			Detail: kvmDevice + " missing",
			Remedy: "Boks needs hardware virtualisation through KVM.\n" +
				"  - On bare metal: enable virtualisation (VT-x/AMD-V/EL2) in firmware and\n" +
				"    load the kvm modules ('lsmod | grep kvm').\n" +
				"  - Inside a VM: enable nested virtualisation on the outer hypervisor.\n" +
				"    Some hypervisors, including Apple Virtualization Framework guests,\n" +
				"    do not expose it at all.",
		}
	case errors.Is(err, fs.ErrPermission):
		return Result{
			Status: StatusFail,
			Detail: kvmDevice + " not accessible",
			Remedy: "You do not have permission to open " + kvmDevice + ".\n" +
				"  Add yourself to the kvm group and start a new login session:\n" +
				"    sudo usermod -aG kvm $USER",
		}
	default:
		return Result{
			Status: StatusFail,
			Detail: kvmDevice + " unusable",
			Remedy: fmt.Sprintf("Opening %s failed: %v", kvmDevice, err),
		}
	}
}

// hypervisorLibrary reports where libkrun is expected on this platform.
func hypervisorLibraryNames() []string {
	return []string{"libkrun.so.1", "libkrun.so"}
}

func hypervisorLibrarySearchPaths() []string {
	paths := []string{"/usr/lib", "/usr/local/lib", "/usr/lib64"}
	// Multiarch layouts vary; probe the common ones rather than parsing ld.so config.
	for _, triple := range []string{"aarch64-linux-gnu", "x86_64-linux-gnu"} {
		paths = append(paths, "/usr/lib/"+triple)
	}
	if extra := os.Getenv("LD_LIBRARY_PATH"); extra != "" {
		paths = append(paths, splitList(extra)...)
	}
	return paths
}
