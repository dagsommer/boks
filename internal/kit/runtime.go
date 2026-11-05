package kit

import (
	"fmt"
	"strconv"
)

// Bytes is the volume's size in bytes, or 0 when it declares none.
func (v Volume) Bytes() (int64, error) {
	if v.Size == "" {
		return 0, nil
	}
	return parseByteSize(v.Size)
}

// Perm is the volume's octal mode, or 0 when it declares none. It carries the sticky/setuid
// bits too, which is why it is not a fs.FileMode: tmpfs takes them as `mode=1777`.
func (v Volume) Perm() (uint32, error) {
	if v.Mode == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(v.Mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("mode %q is not octal", v.Mode)
	}
	return uint32(n), nil
}

// PublishSpecs turns a kit's ports into `--publish` specifications: SANDBOX_PORT with
// HOST_IP and HOST_PORT omitted, so every one lands on an ephemeral loopback port. A kit
// cannot pin a host port: a fetched file does not get to claim a number on the user's machine.
//
// Anything but TCP is skipped and reported in notes, because Boks cannot publish UDP (see the
// ports package); dropping it silently would look like the kit's port was open.
func PublishSpecs(ports []Port) (specs, notes []string) {
	seen := map[string]bool{}
	for _, p := range ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" {
			notes = append(notes, fmt.Sprintf("port %d/%s is not published: Boks publishes TCP only", p.Container, proto))
			continue
		}
		spec := strconv.Itoa(p.Container)
		if seen[spec] {
			continue
		}
		seen[spec] = true
		specs = append(specs, spec)
	}
	return specs, notes
}
