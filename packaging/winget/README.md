# winget

The manifests that put Boks in `winget install boks`, and the part of getting them there
that a repository cannot do for itself.

> [!IMPORTANT]
> **What has been run, and what has not.** On 2026-08-16 the rendered v0.1.1 manifests were
> put through `winget validate` and `winget install --manifest` on Windows 11: the install
> succeeded unelevated in 8.68 s, `boks doctor` was green, and `boks run shell .` booted a
> sandbox. That is recorded in
> [`docs/verification.md`](../../docs/verification.md#winget-delivery-tested-locally-before-any-submission-2026-08-16),
> together with the two things it did *not* establish — no symlink was created, so the
> symlink indirection is still untested, and `winget uninstall dagsommer.boks` by identifier
> failed.
>
> **Nothing has been submitted to `microsoft/winget-pkgs`.** That pipeline re-downloads the
> archive, scans it with multiple AV engines, and installs it as a standard user on a machine
> we do not control; none of that has happened. The local install is the strongest evidence
> here and it is still one machine.

## What is here

| Path | What it is |
|---|---|
| `manifests/dagsommer.boks.yaml.in` | version manifest template |
| `manifests/dagsommer.boks.installer.yaml.in` | installer manifest template |
| `manifests/dagsommer.boks.locale.en-US.yaml.in` | default-locale manifest template |
| `render.sh` | stamps a version, a SHA-256 and a date into the templates |
| `validate.py` | checks the rendered files against winget's JSON schemas |

The templates are templates rather than files-with-a-placeholder-you-edit because a release
must not depend on anyone remembering three substitutions. `{{VERSION}}`,
`{{INSTALLER_SHA256}}` and `{{RELEASE_DATE}}` are the only things a release changes, they
appear nowhere else, and `render.sh` refuses to emit a file that still contains one.

> That last clause became true on 2026-08-16. The guard matched `{{[A-Z_]*}}` — a character
> class with no digits in it — so it could not match `{{INSTALLER_SHA256}}`, the very
> placeholder it is named for, nor any later `{{GUEST_REV_X86_64}}` or `{{SHA256_ARM64}}`. A
> manifest carrying a literal placeholder rendered and the script exited 0. It now matches
> anything between doubled braces, and `.github/workflows/winget.yml` runs a control that
> puts one back to prove the guard still fires.

Template lines beginning `#|` are notes to ourselves and are **stripped at render**, so what
lands in a pull request is a manifest and not our reasoning about it. The rendered files keep
two comment lines: a provenance line and the `yaml-language-server` schema line, which is the
same shape every manifest merged into winget-pkgs carries.

```sh
packaging/winget/render.sh 0.1.0 dist/boks_0.1.0_windows_amd64.zip
packaging/winget/render.sh 0.1.0 A1B2…64-hex-digits…C3D4        # or the digest directly
```

Output lands in `dist/winget/manifests/d/dagsommer/boks/<version>/`, which is the exact path
`microsoft/winget-pkgs` requires, so the rendered tree can be copied over a winget-pkgs
checkout wholesale rather than reassembled by hand.

## The schema version, and why it is not the newest one

`ManifestVersion: 1.12.0`.

This is deliberately *not* the newest schema Microsoft publishes, and picking the newest is
the obvious mistake. The schemas live in **`microsoft/winget-cli`**, not in winget-pkgs
(`winget-pkgs/schemas/JSON/manifests` does not exist and returns 404 — that repository
carries only a validation schema). As of 2026-08-15 winget-cli has frozen schema directories
up to `v1.28.0` and a `latest/` whose `$id` reads `1.30.0`.

The community repository accepts less than that, and says so:

- `winget-pkgs/.github/PULL_REQUEST_TEMPLATE.md` asks contributors to confirm the manifest
  conforms to the **1.12** schema;
- `winget-pkgs/.github/copilot-instructions.md`: *"Recommended schema version: 1.12.0 (1.10.0
  also accepted)"*;
- `winget-pkgs/Tools/YamlCreate.ps1` sets `$ManifestVersion = '1.12.0'`;
- every manifest merged into winget-pkgs on 2026-08-15 that we sampled carries `1.12.0`;
- `winget-pkgs/doc/manifest/README.md` states outright that the community repository *"will
  often delay support for new schema versions until enough devices have been updated"*.

So the rule is: **track what winget-pkgs' own tooling emits, not what winget-cli's `latest/`
contains.** When that changes, three files change together — both `.yaml.in` headers, the
`ManifestVersion:` lines, and `MANIFEST_VERSION` in `validate.py`.

## The installer shape, and the assumption it rests on

`InstallerType: zip` with `NestedInstallerType: portable`.

Boks is a CLI, so `portable` is right. It is a zip rather than a bare portable exe because
the Windows artifact is a **bundle**: Boks orchestrates containerd, the nerdbox shim,
`krun.dll`, `mkfs.erofs.exe` and the guest kernel and root filesystem, and none of those is
packaged anywhere on Windows. A lone `boks.exe` would install a binary that cannot start a
sandbox — the thing `docs/install.md` spends its length warning about.

**`release.yml` now builds exactly that archive.** When this file was first written it did not,
and the manifest named an asset that would have 404'd on the first tag. The `assemble` job
produces `boks_<version>_windows_amd64.zip` as a single top-level directory
`boks_<version>_windows_amd64/` holding `boks.exe`, the runtime beside it, and the guest kernel
and rootfs — and it **asserts that `boks.exe` is at that path** before archiving, so the
contract between this manifest and that workflow fails in our CI rather than in someone else's
repository.

> [!WARNING]
> **The assertion is the only thing holding the two files together, and it is one-directional.**
> It catches `release.yml` laying the archive out differently. Nothing catches someone editing
> `RelativeFilePath` here to a path the workflow does not produce. If the layout has to change,
> the two lines are `InstallerUrl` and `RelativeFilePath` in
> `manifests/dagsommer.boks.installer.yaml.in`, and the check in `release.yml`'s `assemble` job
> has to move with them. Nothing else in this directory depends on the layout.

Why the runtime sitting beside `boks.exe` is the right layout and not a convenience:
`internal/daemon/locate.go` resolves the running executable, follows symlinks, and searches
the directory it lands in — the "tarball, everything side by side" case named in its own
comment. So an extracted bundle needs no configuration, no environment variable and no PATH
entry for Boks to find its own containerd, shim and guest images.

### `ArchiveBinariesDependOnPath` is deliberately absent — and the fallback is the normal case

Setting it `true` tells winget to put the whole extracted directory on the user's `PATH`.
That would place `containerd.exe`, `ctr.exe`, `mkfs.erofs.exe` and `mkfs.ext4.exe` there too,
shadowing a containerd the user installed on purpose — the exact harm `docs/distribution.md`
gives as the reason a `.deb` should put the runtime in `/usr/libexec/boks/` rather than on
`PATH`.

winget's own documentation states what unset means, and it is not "no `PATH` changes":

> Specifying `false` or leaving the value unset will use the default behavior of adding a
> symlink to the `links` folder, if supported, or **adding the install location directly to
> `PATH` if symlinks are not supported**.
>
> — `winget-pkgs/doc/manifest/schema/1.12.0/installer.md`

**This file used to say the fallback was an edge case. It is the ordinary outcome.** On
2026-08-16, on Windows 11 with Developer Mode off and no elevation, `WinGet\Links` was empty
and not on `PATH`; winget created no symlink (`SYMLINK REFUSED: Administrator privilege
required`) and put the package directory on the User `PATH` instead. Three other winget
packages on the same machine were installed the same way. So on a default unprivileged
Windows install, everything in the bundle is on `PATH` today.

Unset is still the right value, because it is the only one under which a machine that *can*
create symlinks gets the clean outcome; `true` would force the shadowing everywhere. But the
accurate description of the package's behaviour is "one alias where symlinks work, the whole
directory on `PATH` where they do not, and the second is what an ordinary user gets".

**Restructuring the archive would fix it, and the winget source says exactly how.** Both
branches in `winget-cli/src/AppInstallerCLICore/PortableInstaller.cpp` put the same thing on
`PATH` — `symlinkTargetPath.parent_path()`, the directory *containing the nested target
executable*, not the package root:

```cpp
// ArchiveBinariesDependOnPath == true
std::filesystem::path installDirectory = symlinkTargetPath.parent_path();
AddToPathVariable(installDirectory);
...
// symlink creation failed
AddToPathVariable(symlinkTargetPath.parent_path());
```

Because the archive is flat, that parent directory is the one holding `containerd.exe`. Lay
it out as `bin/boks.exe` beside `libexec/boks/` and only `bin/` is ever added — the runtime
stays off `PATH` in every branch, including `true`. Boks would still find it: `locate.go`
searches `<exe dir>/../libexec/boks` before the executable's own directory.

That change is **not** made here, because it would mean the manifest described an archive
different from the one already published and install-tested at v0.1.1. It is a release-layout
decision, and three things move together when it is taken: `RelativeFilePath` here, the layout
in `release.yml`'s `assemble` job, and that job's assertion on the path. What *is* fixed here
is this file, which claimed the shadowing needed "a symlink failure first" as though that were
rare. It is the default outcome on an unprivileged machine.

### Other choices

- **x64 only.** There is no Windows arm64 build: libkrun's WHP backend here is x86_64 and so
  is the guest kernel that has booted under it.
- **`MinimumOSVersion: 10.0.22000.0`** — Windows 11 RTM. The Windows Hypervisor Platform is
  older than that, but Windows 11 is the only thing this stack has ever run on, so the floor
  stated is the floor observed.
- **No `Copyright`.** The repository's `LICENSE` is the plain Apache-2.0 text with no
  copyright line in it. The field is optional and inventing a value would assert something
  no file in this project asserts.
- **No `Icons`.** winget-pkgs populates that field in its own validation pipeline; it is not
  authorable in a pull request.
- **No `InstallModes`, `UpgradeBehavior` or `InstallationMetadata`.** None is meaningful for
  an archive of portables, and the merged manifests we modelled this on omit all three.

## What has actually been checked

`validate.py` runs on Linux and does two things winget itself would do on Windows, and
stops well short of the rest.

**1. Schema validation.** winget's manifest schemas are ordinary draft-07 JSON Schema, so the
same documents `winget validate` checks against can be checked against here. Run on
2026-08-15 against `v1.12.0` of all three schemas, all three rendered manifests passed.

**2. The conditional rules the schema cannot express.** The installer schema contains no
`if`/`then`/`allOf`, so a generic JSON-schema validator cannot see any of winget-pkgs'
cross-field rules. Four are checked by hand in `cross_file_problems`, each quoting the
documentation it comes from: that a `zip` needs a `NestedInstallerType` and
`NestedInstallerFiles`; that more than one nested file is allowed only for `portable`; that
`PortableCommandAlias` is valid only for `portable`; and that the three files agree on
identifier, version and manifest version.

Both were exercised with a **negative control** — a copy of the rendered manifests mutated to
`InstallerType: tarball`, `NestedInstallerType: exe` and two nested files — and the validator
reported all three faults and exited non-zero. A validator that passes everything proves
nothing, which is why that run happened.

**3. That all three manifests are there, and that they declare the schema version actually
fetched.** Both added 2026-08-16, and both were holes rather than omissions:

- a *missing* manifest was a silent pass. Only an entirely empty directory failed, so a
  render that produced one file out of three reported `ok` — and every cross-file rule above
  is vacuously satisfied over a set of one. Reproduced by deleting
  `dagsommer.boks.installer.yaml` from a good render: exit 0.
- `ManifestVersion` was compared only *across* the three documents. One `sed` pass stamps all
  three from templates carrying the same literal, so that comparison cannot fail — while
  `MANIFEST_VERSION` in `validate.py`, the constant the schema URL is built from, was never
  compared to them at all. Reproduced by rewriting all three to `1.9.0`: the validator
  fetched the 1.12.0 schemas, printed `ok against manifest.installer.1.12.0`, and exited 0.

**Where these run.** `.github/workflows/winget.yml` renders and validates on every change
under `packaging/winget/`, with a negative control for each defect above, and sets
`BOKS_WINGET_REQUIRE_VALIDATION=1` so that render.sh's graceful "jsonschema not available,
skipped schema validation" becomes a failure rather than a green run that checked nothing.
`make winget` does the same render locally. Before that, nothing ran either script: they were
documented here and in `docs/distribution.md` and invoked by no workflow, no Makefile target
and no test, so the first thing to exercise them would have been a release.

**4. The house style winget-pkgs enforces on every file in it.** Added 2026-08-17, after an
audit against `doc/README.md#authoring-a-manifest` and everything it links. Two of these are
things a JSON schema cannot see:

- **CRLF line endings.** winget-pkgs' `.editorconfig` sets `end_of_line = crlf` for `[*]`,
  exempting only three spell-check text files, and every manifest merged there is CRLF on the
  wire — checked against `BurntSushi.ripgrep.MSVC` 14.1.1 and `sharkdp.fd` 10.4.2, the latter
  written by komac at ManifestVersion 1.12.0. `render.sh` now emits CRLF. UTF-8 **without** a
  BOM, which is what those two files are and what `YamlCreate.ps1` writes
  (`$Utf8NoBomEncoding`).
- **Field order follows the JSON schema's property order**, which is also komac's
  serialisation order. Two keys were out of place and are now fixed; the reasoning is in the
  templates.

**What a pass does not mean.** It does not mean winget-pkgs' pipeline will accept the
submission — that pipeline is ten steps long and only one of them is a schema check. Three
things this validator cannot see were settled by the 2026-08-16 install instead of by
inference: the URL resolves, the SHA-256 matches the archive at it, and `boks.exe` is at the
declared path inside it. One thing it still cannot see, and neither could that install: the
symlink was never created, so nothing has yet exercised the indirection.

## Submitting to microsoft/winget-pkgs

This is a pull request to somebody else's repository, reviewed by people who are not us. No
amount of work in this directory removes the human steps, and pretending otherwise is how a
release plan acquires a step nobody owns.

### What a human has to do, in order

1. **Sign Microsoft's CLA.** Tracked separately from validation; a `Needs-CLA` label blocks
   the merge regardless of whether everything else passed.
2. **Cut the release first.** The manifest names an `InstallerUrl` and an `InstallerSha256`
   for an asset that must already be downloadable — the pipeline re-downloads the archive and
   compares the hash. A draft release is not publicly downloadable and will fail.
3. **Render, and check the digest against the release's `SHA256SUMS`.** `render.sh` computes
   it from a local file if you give it one, which is the transcription step worth removing.
4. **Fork `microsoft/winget-pkgs`**, copy the rendered `manifests/d/dagsommer/boks/<version>/`
   directory into the fork at that same path, and open one pull request.
   - **One package version per pull request.** Two versions in one PR is rejected.
   - **Manifest files only.** No README, tooling or spelling changes mixed in.
   - Multi-file only; `ManifestType: singleton` is deprecated and rejected.
5. **Wait for the validation checks.** They run as a GitHub App posting check runs — the
   Azure DevOps pipeline this used to be is gone, though the `Azure-Pipeline-Passed` label
   name survives it. Ten steps, of which the ones this package can plausibly trip are listed
   below. A moderator can re-trigger them by commenting `@wingetbot run`.
6. **Wait for a human moderator.** Every pull request needs one, not just new packages;
   approval applies `Moderator-Approved` and the merge then happens automatically. Infrequent
   contributors get more scrutiny, and a first-ever package from an unknown publisher is as
   infrequent as it gets. If it sits more than a day, the documented remedy is to ping a
   moderator.
7. **Do the next one by hand too.** This step used to say "then automate it with
   `vedantmgoyal9/winget-releaser`". That was read from the action's README and is now
   read from its source, and the source says the automation cannot author *this* package.
   See [Automating later versions](#automating-later-versions-and-why-winget-releaser-is-not-wired-in-yet).

### What we should expect to go wrong

- **URL reputation, not code signing.** winget-pkgs requires no signature for a zip; only
  MSIX must be signed. But the URL validation step fails a URL that Microsoft Defender
  SmartScreen reports as **low reputation**, which a brand-new unsigned release is a
  candidate for, and the installer scan step runs multiple AV engines over the archive.
  Policy on that is categorical: a package flagged by any security scan cannot be accepted
  *"regardless of the application's legitimacy or intent"*. The mitigations are to host on
  GitHub Releases rather than a new domain — which we do — and, if it trips, to submit the
  URL to Microsoft at `microsoft.com/wdsi/filesubmission/`.
- **Provenance.** The `InstallerUrl` must be the publisher's own release location; download
  aggregators, mirrors and URL shorteners are refused, and redirects are resolved to the
  final URL. A GitHub release asset on `dagsommer/boks` satisfies this. `PackageUrl` is set
  so a moderator can confirm it in one click.
- **A package this unusual invites questions.** Boks installs a hypervisor library and starts
  a daemon. Nothing in winget's policies prohibits that, but it is worth having
  `docs/verification.md` and `docs/security-model.md` ready to link, which is why they are in
  `Documentations`.

Ownership is *not* a problem: winget-pkgs is a community repository and third parties submit
packages they do not own routinely. What it enforces is the provenance of the URL, not the
identity of the submitter.

## Automating later versions, and why winget-releaser is not wired in yet

`vedantmgoyal9/winget-releaser` is the obvious candidate: a GitHub Action that drives
[Komac](https://github.com/russellbanks/Komac) to open the winget-pkgs pull request from a
published release. It is **not** in `.github/workflows/`, and the reason is not caution in
general — it is a specific, sourced defect that would produce an invalid manifest for this
package every single time.

### It cannot make the first submission. Confirmed, three ways.

1. Its README: *"At least **one** version of your package should already be present in the
   Windows Package Manager Community Repository. The action will use that version as a base
   to create manifests for new versions of the package."*
2. `action.yml`'s first step is a `HEAD` request against
   `github.com/microsoft/winget-pkgs/tree/master/manifests/<d>/<Publisher>/<Package>` and
   `exit 1` with *"Package … does not exist in the winget-pkgs repository"* if it 404s.
3. It runs `komac update`, never `komac new`. Komac's own help strings are
   *"Add a version to a pre-existing package"* against *"Create a package from scratch"*, and
   `update` resolves the base through `get_versions()`, which maps a missing package to
   `PackageNonExistent`. (`komac new` would work, but it is interactive — `inquire` prompts
   throughout — so it is not a CI path either.)

**So step 4 above is a human step at least once, and that is not the reason it stays human.**

### The blocking defect: it cannot describe this archive

Komac regenerates `Installers` wholesale from binary analysis on every run. For a zip it uses
`Zip::new` (`src/analysis/installers/zip.rs`), which auto-detects the nested installer **only
when exactly one extension category in the archive contains exactly one file**:

```rust
if installer_type_counts.values().filter(|&&count| count == 1).count() == 1 {
```

`boks_0.1.1_windows_amd64.zip` contains **six** `.exe` files — measured by listing the
published archive, whose SHA-256 matches the release's `SHA256SUMS`:

```
boks.exe  containerd.exe  ctr.exe  containerd-shim-nerdbox-v1.exe  mkfs.erofs.exe  mkfs.ext4.exe
```

The count for `exe` is 6 and every other category is 0, so no category has exactly 1, the
condition is false, and the fallback runs:

```rust
installers: installers.unwrap_or_else(|| vec![Installer {
    r#type: Some(InstallerType::Zip),
    nested_installer_files,      // still the empty BTreeSet
    ..Installer::default()
}]),
```

That is `InstallerType: zip` with **no `NestedInstallerFiles`** — which the schema requires
for an archive type, and which `validate.py`'s own cross-file check would reject. The
interactive `Zip::prompt()` is what picks the file and asks for a `PortableCommandAlias`, and
`update_version.rs` never calls it; on the automatic path `portable_command_alias` is
hard-coded `None`. So even in the one-exe case we would silently lose `PortableCommandAlias:
boks`, the field that makes `boks` a command at all.

**This is not fixable by archive layout.** The count is over every entry in the zip, whatever
directory it sits in, so the `bin/` + `libexec/` restructure discussed above does not change
it. The count only drops to one if the archive stops carrying the runtime — which is the
whole reason this package exists.

### What it does get right, for whenever the shape changes

Read from `winget-types/src/manifests/locale/mod.rs` and Komac's `LocaleExt::update`, the
hand-written locale fields **are** carried forward from the previous version verbatim:
`InstallationNotes`, `Documentations`, `Description`, `ShortDescription`, `Moniker`,
`License`, `PackageName`, `Publisher`. `Tags` survives too — it is only backfilled from the
GitHub repository's topics when already empty. `PublisherUrl`, `PublisherSupportUrl`,
`PackageUrl` and `LicenseUrl` are likewise only filled when unset.

Two are *not* preserved: `ReleaseNotes` and `ReleaseNotesUrl` are overwritten on every run
from the GitHub release body, and **deleted** if the release has no body. Ours is set from a
template, so it would be replaced by Komac's equivalent rather than lost — but note that our
version-pinned `LicenseUrl` and `Documentations` would survive, which is the outcome we want.
`ReleaseDate` is set automatically from each asset's HTTP `Last-Modified` header, not from the
release's publish date.

### If it is ever wired in, these are the terms

- **Token: a *classic* PAT with `public_repo`.** Fine-grained PATs do not work, and this is a
  GitHub platform limitation rather than an action bug: a fine-grained token's resource owner
  must match the resource's owner, and the resource here is `microsoft/winget-pkgs`. Komac
  documents the same constraint independently. The workflow `GITHUB_TOKEN` cannot work either
  — it is scoped to this repository, and the action needs to push to a fork and open a PR
  against a third repository.
- **Blast radius if it leaks:** `public_repo` is *"read/write access to code, commit statuses,
  repository projects, collaborators, and deployment statuses for **public** repositories"* —
  every public repository the token's owner can push to, including `dagsommer/boks` itself and
  its releases. Classic PATs have no per-repository selector, so this cannot be narrowed.
  **Use a dedicated bot account** whose only write access is its own winget-pkgs fork.
- **A fork of `microsoft/winget-pkgs` must already exist** under the token owner's account,
  named exactly `winget-pkgs`. Nothing in Komac creates it — there is no `create_fork` call in
  the source. `fork-user` selects whose fork to use.
- **`installers-regex` must be set.** Its default is
  `.(exe|msi|msix|appx)(bundle){0,1}$`, which matches no asset we publish. Worse, a loose
  `\.zip$` would match **two** assets — `boks_<v>_windows_amd64.zip` and
  `boks-runtime_<v>_windows_amd64.zip` — and Komac hard-errors on an asset extension it cannot
  parse rather than skipping it.
- **Pin it by commit SHA, not by tag.** `v2` is the repository's only tag and points at a
  commit from 2024-11-27; `main` has moved several times since, and the action's own README
  tells you to use `@main`. Note that pinning the action does **not** pin what it runs: it
  invokes `cargo-bins/cargo-binstall@main` and then `cargo-binstall komac -y` with no version
  constraint, so the Komac binary that authors the manifest is whatever is newest that day.
- **Leave `max-versions-to-keep` at `0`.** Above zero it runs `komac remove --submit`, opening
  deletion pull requests against the community repository under your name.

None of this is currently exercised by anything in this repository, and none of it should be
turned on without a person deciding to.

## What is still missing before any of this can be submitted

Both items that used to be on this list are done. `v0.1.1` is published and not a draft, so
the `InstallerUrl` resolves and the digest is computable — and `mkfs.ext4.exe` is built and
is in the archive, so the pre-formatted writable-layer workaround is gone. Listing the
archive at that URL confirms both, and its SHA-256 matches the release's `SHA256SUMS`.

What remains is not a missing artefact:

1. **A signed CLA and a fork**, neither of which a repository can do for itself.
2. **The manifests have never met winget-pkgs' pipeline.** They have met `winget validate`
   and `winget install --manifest` on one Windows 11 machine. The pipeline additionally
   re-downloads the 58 MB archive, runs multiple AV engines over it, and installs *and
   uninstalls* it as a standard user in an environment we do not control.
3. **Two known risks that a local install could not surface**, both worth expecting rather
   than being surprised by:
   - `winget uninstall dagsommer.boks` failed by identifier in the local test, and the
     pipeline exercises uninstall (`Validation-Uninstall-Error`). It was inferred there that
     this is an artefact of `--manifest` installs having no source to resolve against; that
     inference is exactly what a real submission tests.
   - The pipeline's install validation runs in an isolated environment, and
     `Tools/SandboxTest.ps1` runs in **Windows Sandbox**, which is itself a VM. Boks needs
     the Windows Hypervisor Platform. `boks.exe` will install and be discoverable there, but
     `boks run` cannot be expected to work inside it, and a moderator who tries will see that.
     The moderator labels to be ready for are `Portable-Archive` (*"the installer is a
     portable archive that may need special handling"*) and `Hardware`.

**The guest is in the archive**, which is what makes a `winget install` boot a sandbox with no
further download and is why this package is worth submitting at all. The kernel is GPL-2.0, so
the archive carries a `SOURCE.txt` naming the exact nerdbox revision and the commands that
reproduce it; the reasoning, including what that does and does not settle, is in
`docs/distribution.md` under "What goes in the Windows archive, and the guest".
