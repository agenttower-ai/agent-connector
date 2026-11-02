# Releasing Connector

The Release Connector workflow tests on Windows, macOS and Linux, cross-compiles six static binaries, packages licenses and publishes ZIPs plus SHA256SUMS.txt. All jobs must pass before a public release is published.

Use Actions → Release Connector → Run workflow on `main` and enter a new tag such as `v0.2.15`, or push a new `vMAJOR.MINOR.PATCH` tag. A manual run creates its tag at the checked-out commit only after successful builds. Never reuse a published version. Tags with a suffix such as `-rc.1` produce prereleases.

If an upload fails after draft creation, remove the incomplete draft before rerunning the same tag workflow. Do not move published tags. Windows/macOS binaries are not commercially signed. Keep the full LICENSE and dependency notices when redistributing.

Local packaging: `python3 scripts/build_release.py v0.2.15`. Requires Go, Git and Python 3. Output is in `dist/`; it includes no local configuration or device credentials.
