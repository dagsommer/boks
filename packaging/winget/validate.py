#!/usr/bin/env python3
"""Validate rendered winget manifests against winget's own JSON schemas.

    packaging/winget/validate.py dist/winget/manifests/d/dagsommer/boks/0.1.0

This exists because the real validator, `winget validate`, runs only on Windows and nobody
on this project has a Windows machine to run it on. What this does instead is the part that
is portable: winget's manifest schemas are ordinary draft-07 JSON Schema, published in
microsoft/winget-cli, so the same documents winget checks against can be checked against
here.

Be precise about what a pass means and does not mean. It means ALL THREE files are present,
parse as YAML, carry the fields the schema requires, violate none of its patterns, enums or
length limits, agree with each other about which package version they describe, and declare
the ManifestVersion whose schemas were actually fetched. It does not mean winget will install
the package, that the installer URL resolves, that the SHA-256 matches the archive at that
URL, that the nested path exists inside the archive, or that winget-pkgs' own pipeline will
accept the submission. Those are runtime facts and this is a document check.

Two of those clauses are recent, and both were holes rather than omissions. Until 2026-08-16
a *missing* manifest was a silent pass — only an entirely empty directory failed — so a
render that produced one file out of three reported ok, and every cross-file rule was
vacuously satisfied over a set of one. And ManifestVersion was compared only across the three
documents, which one `sed` pass stamps from templates carrying the same literal: a
tautology that held no matter what the value was, while MANIFEST_VERSION here — the constant
the schema URL is built from — was never compared to them at all.

The schemas are fetched from the network on first use and cached under the directory named
by BOKS_WINGET_SCHEMA_CACHE, defaulting to a temporary directory. Nothing is committed:
they are Microsoft's files, they move, and a stale copy vendored here would validate
against a version of the truth rather than the truth.
"""

from __future__ import annotations

import datetime
import json
import os
import pathlib
import sys
import tempfile
import urllib.request

import jsonschema
import yaml

# The version winget-pkgs' own Tools/YamlCreate.ps1 stamps into new manifests, and the one
# every manifest merged into that repository carries today. It is deliberately not the
# newest schema in winget-cli — that repository's latest/ runs ahead of what the community
# repository accepts — so this constant tracks winget-pkgs, and the templates and this file
# must be changed together.
MANIFEST_VERSION = "1.12.0"

SCHEMA_BASE = (
    "https://raw.githubusercontent.com/microsoft/winget-cli/master/schemas/JSON/manifests"
    f"/v{MANIFEST_VERSION}/manifest.{{kind}}.{MANIFEST_VERSION}.json"
)

# Which schema each rendered file is checked against, keyed by the suffix of its name.
KINDS = {
    ".installer.yaml": "installer",
    ".locale.en-US.yaml": "defaultLocale",
    ".yaml": "version",
}


def schema_for(kind: str) -> dict:
    cache = pathlib.Path(
        os.environ.get("BOKS_WINGET_SCHEMA_CACHE")
        or pathlib.Path(tempfile.gettempdir()) / "boks-winget-schemas"
    )
    cache.mkdir(parents=True, exist_ok=True)
    path = cache / f"manifest.{kind}.{MANIFEST_VERSION}.json"
    if not path.exists():
        url = SCHEMA_BASE.format(kind=kind)
        with urllib.request.urlopen(url, timeout=30) as response:
            path.write_bytes(response.read())
    return json.loads(path.read_text(encoding="utf-8"))


def as_winget_types(node):
    """Re-type a PyYAML document the way winget's own YAML reader types it.

    PyYAML resolves an unquoted `2026-08-15` to a `datetime.date`; winget reads it as a
    string with `format: date`, which is why every merged manifest writes it unquoted. Left
    alone, the schema check would reject a correct manifest, so the fix belongs here rather
    than in the template.
    """
    if isinstance(node, dict):
        return {key: as_winget_types(value) for key, value in node.items()}
    if isinstance(node, list):
        return [as_winget_types(value) for value in node]
    if isinstance(node, (datetime.datetime, datetime.date)):
        return node.isoformat()
    return node


def kind_of(name: str) -> str | None:
    # Longest suffix first: every manifest name ends in .yaml, so the specific ones have to
    # be tried before the generic one or everything validates as a version manifest.
    for suffix in sorted(KINDS, key=len, reverse=True):
        if name.endswith(suffix):
            return KINDS[suffix]
    return None


def cross_file_problems(documents: dict) -> list[str]:
    """The rules the JSON schema cannot express, checked by hand.

    winget's installer schema contains no `if`/`then`/`allOf`, so the conditional rules in
    winget-pkgs' own documentation are invisible to any generic validator. These four are
    the ones this package can actually violate, and each of them fails in someone else's
    repository rather than here if it is missed.
    """
    problems = []

    installer = documents.get("installer", {})
    root_nested_type = installer.get("NestedInstallerType")

    for index, entry in enumerate(installer.get("Installers") or []):
        where = f"installer.Installers[{index}]"
        installer_type = entry.get("InstallerType", installer.get("InstallerType"))
        nested_type = entry.get("NestedInstallerType", root_nested_type)
        nested_files = entry.get("NestedInstallerFiles") or installer.get("NestedInstallerFiles")

        # "NestedInstallerType and NestedInstallerFiles are required when InstallerType is
        # an archive type such as .zip" — doc/manifest/schema/1.12.0/installer.md.
        if installer_type in ("zip",):
            if not nested_type:
                problems.append(f"{where}: InstallerType zip needs a NestedInstallerType")
            if not nested_files:
                problems.append(f"{where}: InstallerType zip needs NestedInstallerFiles")

        if nested_files and nested_type != "portable":
            # "This field can only contain one nested installer file unless the
            # NestedInstallerType is portable."
            if len(nested_files) > 1:
                problems.append(
                    f"{where}: only NestedInstallerType portable may list more than one "
                    f"nested installer file (got {len(nested_files)})"
                )
            # "PortableCommandAlias is only valid when NestedInstallerType is portable."
            if any(file.get("PortableCommandAlias") for file in nested_files):
                problems.append(
                    f"{where}: PortableCommandAlias is only valid for "
                    f"NestedInstallerType portable"
                )

    # ALL THREE documents, not "however many happened to be there". A winget-pkgs submission
    # is a set of three and is rejected as incomplete without them; more to the point, every
    # cross-file check below is vacuous over a set of one, so a missing manifest used to be a
    # silent pass here. The only failure was an EMPTY directory.
    missing_kinds = sorted(set(KINDS.values()) - set(documents))
    if missing_kinds:
        problems.append(
            "the manifest set is incomplete: no "
            + ", ".join(f"{kind} manifest" for kind in missing_kinds)
        )

    # The three files describe one package version. winget-pkgs rejects a set that
    # disagrees with itself, and a rendering bug is exactly how they come to disagree.
    for field in ("PackageIdentifier", "PackageVersion", "ManifestVersion"):
        values = {kind: document.get(field) for kind, document in documents.items()}
        if len(set(values.values())) > 1:
            problems.append(f"{field} differs across the manifest set: {values}")

    # And ManifestVersion against the constant this file validates AGAINST, which is the
    # comparison that can actually fail. The three documents come out of one `sed` pass over
    # templates that all carry the same literal, so "they agree with each other" is a
    # tautology — it was true of a set whose ManifestVersion disagreed with the schemas being
    # fetched, which is the mistake worth catching: the schema URL is built from
    # MANIFEST_VERSION, so a template bumped without this constant would be checked against
    # the wrong schema and pass.
    for kind, document in sorted(documents.items()):
        declared = document.get("ManifestVersion")
        if declared != MANIFEST_VERSION:
            problems.append(
                f"{kind} manifest declares ManifestVersion {declared!r} but this validator "
                f"fetched the {MANIFEST_VERSION} schemas; the templates and validate.py's "
                f"MANIFEST_VERSION must be changed together"
            )

    return problems


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print(__doc__.strip().splitlines()[2].strip(), file=sys.stderr)
        return 2

    directory = pathlib.Path(argv[1])
    manifests = sorted(directory.glob("*.yaml"))
    if not manifests:
        print(f"validate.py: no manifests in {directory}", file=sys.stderr)
        return 1

    failed = False
    documents = {}
    for manifest in manifests:
        kind = kind_of(manifest.name)
        if kind is None:
            print(f"validate.py: {manifest.name}: unrecognised manifest kind", file=sys.stderr)
            failed = True
            continue

        try:
            parsed = yaml.safe_load(manifest.read_text(encoding="utf-8"))
        except yaml.YAMLError as exc:
            # A traceback is not a diagnosis. An unsubstituted `{{PLACEHOLDER}}` parses as a
            # YAML flow mapping with an unhashable key and used to crash this script with a
            # stack trace from PyYAML's constructor.
            failed = True
            print(f"{manifest.name}: is not valid YAML: {exc}", file=sys.stderr)
            continue
        if not isinstance(parsed, dict):
            failed = True
            print(
                f"{manifest.name}: is not a YAML mapping (got {type(parsed).__name__})",
                file=sys.stderr,
            )
            continue
        document = as_winget_types(parsed)
        documents[kind] = document
        errors = sorted(
            jsonschema.Draft7Validator(schema_for(kind)).iter_errors(document),
            key=lambda error: list(error.path),
        )
        if errors:
            failed = True
            for error in errors:
                where = "/".join(str(part) for part in error.path) or "(root)"
                print(f"{manifest.name}: {where}: {error.message}", file=sys.stderr)
        else:
            print(f"{manifest.name}: ok against manifest.{kind}.{MANIFEST_VERSION}")

    for problem in cross_file_problems(documents):
        failed = True
        print(f"validate.py: {problem}", file=sys.stderr)

    if failed:
        return 1

    print(
        "schema-valid only. Not run: winget validate, winget install, "
        "or winget-pkgs' submission pipeline."
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
