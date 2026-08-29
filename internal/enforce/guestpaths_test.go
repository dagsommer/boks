package enforce

import (
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/secret"
)

// guestPrepareDir is where the images put their startup scripts: images/base/Dockerfile
// creates it and boks-prepare runs what is in it before the agent starts. It is part of the
// IMAGE, not a mount, which is exactly why a mount must not land on top of it.
const guestPrepareDir = "/etc/boks/prepare.d"

// Nothing Boks shares into the guest may contain anything else Boks puts there.
//
// Both halves of this were live defects, from one cause — the CA was mounted at /etc/boks,
// the whole namespace:
//
//   - it replaced the image's /etc/boks/prepare.d, so those scripts never ran, and nothing
//     said so;
//   - it made /etc/boks/credentials unmountable, because the runtime has to create that
//     mount point inside the read-only CA mount: "mkdir `/etc/boks/credentials`: Read-only
//     file system", reported from macOS on 2026-08-29.
//
// A containment check rather than an equality check, so that this keeps holding for a path
// added later by someone who has not read any of the above.
func TestGuestPathsDoNotNest(t *testing.T) {
	paths := map[string]string{
		"the CA mount":          GuestCADir,
		"the credential mount":  secret.GuestCredentialDir,
		"the image's prepare.d": guestPrepareDir,
	}
	for anName, a := range paths {
		for bName, b := range paths {
			if anName == bName {
				continue
			}
			if a == b {
				t.Errorf("%s and %s are the same path %q; one would hide the other", anName, bName, a)
				continue
			}
			if strings.HasPrefix(b, a+"/") {
				t.Errorf("%s (%s) is inside %s (%s).\n"+
					"A read-only mount at the outer path hides the inner one, and a mount at the\n"+
					"inner path cannot be created inside it. Give each its own directory.",
					bName, b, anName, a)
			}
		}
	}
}

// And each must still be under the namespace, since that is what makes them recognisable
// inside a guest and what the docs tell people to look for.
func TestGuestPathsStayUnderTheBoksNamespace(t *testing.T) {
	for name, p := range map[string]string{
		"the CA mount":         GuestCADir,
		"the credential mount": secret.GuestCredentialDir,
	} {
		if !strings.HasPrefix(p, "/etc/boks/") {
			t.Errorf("%s is %q, outside /etc/boks/", name, p)
		}
	}
}
