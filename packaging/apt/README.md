# Debian/Ubuntu repository signing

`apt` refuses an unsigned repository outright, so a `.deb` archive needs an OpenPGP key
before it needs anything else. This directory holds the public half.

## The key

| | |
| --- | --- |
| Fingerprint | `D5DD07C0F9589C164F7361C20EB93D3C39471E1E` |
| Type | RSA 4096, sign-only |
| Created | 2026-08-13, expires 2029-08-13 |
| Public half | [`boks-archive-keyring.asc`](boks-archive-keyring.asc) — committed here, it is meant to be published |
| Private half | GitHub Actions secret `APT_GPG_PRIVATE_KEY`, with the fingerprint in `APT_GPG_KEY_FPR` |

The private key was generated with `%no-protection` — no passphrase. That is deliberate for
an unattended CI signer, where a passphrase would only move the secret rather than protect
it: anything that can read the passphrase secret can read the key secret beside it.

## What this key is and is not

It signs **repository metadata**, so that apt can tell that the index it fetched is the one
we published. It is not a statement about the contents of a package, and it is not a code
signing certificate — see `docs/install.md` for the macOS entitlement signing and the
Windows SmartScreen story, which are separate problems with separate answers.

## Provenance, which matters more here

Signing proves the archive came from whoever holds this key. It does not prove the binary
came from the source. Releases therefore also carry
[build provenance](https://docs.github.com/actions/security-guides/using-artifact-attestations)
attesting the commit and workflow run that produced each artifact, which is verifiable with
`gh attestation verify` and does not require trusting a key at all.

## Using it

```sh
curl -fsSL https://raw.githubusercontent.com/dagsommer/boks/main/packaging/apt/boks-archive-keyring.asc \
  | sudo tee /usr/share/keyrings/boks-archive-keyring.asc >/dev/null
echo "deb [signed-by=/usr/share/keyrings/boks-archive-keyring.asc] https://dagsommer.github.io/boks/apt stable main" \
  | sudo tee /etc/apt/sources.list.d/boks.list
sudo apt update && sudo apt install boks
```

`signed-by` scopes the key to this one repository. A key dropped in `trusted.gpg.d`
instead would be trusted to sign *any* repository on the system, including ones it has
nothing to do with — a footgun that predates `signed-by` and still gets copied around.

## Rotation

The expiry is real: when it passes, `apt update` starts failing for everyone who added the
key. Rotate well before 2029, and expect to publish both keys for a period so clients that
have not updated keep working.

To rotate, or if the key is ever suspected compromised:

```sh
gpg --batch --gen-key <<'EOF'
%no-protection
Key-Type: RSA
Key-Length: 4096
Key-Usage: sign
Name-Real: Boks Archive Signing Key
Name-Comment: apt/rpm repository signing
Name-Email: dagaleks@gmail.com
Expire-Date: 3y
%commit
EOF
gpg --armor --export "$FPR" > boks-archive-keyring.asc
gpg --armor --export-secret-keys "$FPR" | gh secret set APT_GPG_PRIVATE_KEY -R dagsommer/boks
gh secret set APT_GPG_KEY_FPR -R dagsommer/boks --body "$FPR"
```

Then delete every local copy of the private half. The current key was generated this way in
an ephemeral keyring which was shredded immediately afterwards, so the GitHub secret is the
only copy — losing it means rotating, not recovering, which is the correct trade for a key
that signs an archive nobody has installed yet.
