package cli

import "testing"

// A destination written with a trailing slash names a directory, on either side.
func TestCopyDestinationWithATrailingSlashIsADirectory(t *testing.T) {
	up, err := parseCopyArgs("./src", "web:/new/dir/")
	if err != nil || !up.DestIsDir || up.GuestPath != "/new/dir/" {
		t.Errorf("upload: %+v, %v", up, err)
	}
	down, err := parseCopyArgs("web:/var/log/app", "./out/")
	if err != nil || !down.DestIsDir {
		t.Errorf("download: %+v, %v", down, err)
	}
	plain, err := parseCopyArgs("./src", "web:/new/name")
	if err != nil || plain.DestIsDir {
		t.Errorf("a path without a trailing slash: %+v, %v", plain, err)
	}
}
