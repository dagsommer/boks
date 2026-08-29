package enforce

import (
	"context"
	"fmt"
	"os"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

// openRotationStore returns somewhere to persist a refreshed OAuth credential, or nil.
//
// The supervisor is deliberately given resolved VALUES on a pipe rather than a way into the
// credential store, so that a long-lived background process cannot read every credential the
// user owns. Writing back one credential it already holds is a much smaller thing than that,
// and without it a rotation is thrown away — see rotationHandler for what that costs.
//
// The keyring only, never the encrypted file: opening the file needs a passphrase, and the
// passphrase is exactly what this process is not given. A host with no keyring therefore
// keeps the old behaviour, which is why this returns nil rather than an error.
func openRotationStore(stateDir string) secret.OAuthSaver {
	// Asked for the file explicitly, so there is nothing here to write to. Checked before
	// the keyring is probed, so that saying "not the keyring" cannot itself raise the
	// prompt that answer exists to avoid.
	if os.Getenv(secret.DisableKeyringEnv) != "" {
		return nil
	}
	ring, err := secret.OpenKeyring(context.Background())
	if err != nil {
		return nil
	}
	return secret.NewKeyringStore(ring, secret.DefaultIndexPath(stateDir))
}

// rotationHandler returns the callback the sandbox's store invokes after a refresh.
//
// # What this is for
//
// Providers that rotate refresh tokens — most of them, Anthropic among them — invalidate the
// old pair at the moment of exchange. So a refresh performed inside a sandbox does not merely
// fail to help the next one: it BREAKS the copy on the host, which now holds a token the
// provider has already retired. Every later sandbox then fails its first refresh, and the
// agent reports it as a network problem.
//
// Measured on 2026-08-29: a sandbox refreshed at 08:00, the note said the host's copy was now
// stale, and an hour later a fresh sandbox could not reach api.anthropic.com at all. The
// user's policy log showed every rule allowing and nothing blocked, because nothing was.
//
// # Why the store is written from here
//
// internal/secret's MemoryStore said the fix was "either a writeback channel to the process
// that has [the passphrase], or refreshing in the CLI before the supervisor is spawned;
// neither is built". The premise expired: credentials live in the OS keyring now, which needs
// no passphrase and is reachable by the same user, and KeyringStore.SaveOAuth already
// existed. So the writeback is direct.
//
// # What it does when it cannot
//
// It says so, in the decision log, with the remedy — because a credential the user believes
// is stored and is not is the failure this exists to prevent, and it must not be silent.
func rotationHandler(spec Spec, store *secret.MemoryStore, saver secret.OAuthSaver,
	engine *policy.Engine, logf func(string, ...any)) func(string) {
	return func(service string) {
		target, terr := policy.NewTarget(spec.OAuth[service].TokenHost, 443)
		if terr != nil {
			return
		}
		note := func(reason string) {
			engine.Note(policy.StageRequest, target, policy.ModeForward, reason)
		}

		if saver == nil {
			note("the oauth credential " + service + " was refreshed for this sandbox only; this " +
				"host has no keyring to store it in, so the copy on the host is now stale — " +
				"re-run 'boks secret import' when this sandbox ends")
			return
		}

		// Read back rather than being handed the tokens: the store is the one that knows
		// what it now holds, and a rotation that raced another would otherwise persist
		// the older pair.
		record, ok := store.Records()[service]
		if !ok {
			note("the oauth credential " + service + " was refreshed but is no longer in this " +
				"sandbox's store; the copy on the host may be stale — re-run 'boks secret import'")
			return
		}

		if err := saver.SaveOAuth(context.Background(), service, record.Tokens()); err != nil {
			// Never the tokens, only that it failed. The error comes from the keyring
			// and names the service at most.
			logf("storing the refreshed %s credential: %v", service, err)
			note(fmt.Sprintf("the oauth credential %s was refreshed but could not be stored (%v). "+
				"The provider has already retired the old token, so the copy on the host will "+
				"not work: re-run 'boks secret adopt' or 'boks secret import'", service, err))
			return
		}
		note("the oauth credential " + service + " was refreshed and the stored copy updated")
	}
}
