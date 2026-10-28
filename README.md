# Argus Connect — Android alpha

Local Android network recovery (LIGHT) and a separate VPN-protected hotspot.
**Experimental alpha; connectivity depends on the carrier, endpoints and OS.**

- LIGHT uses Android VpnService and a userspace TCP/UDP stack. It does not require root.
- Hotspot can forward through the VPN currently active on the phone, including an external VPN with LIGHT disabled. A privileged, user-authorized Android bridge is required. Mobile-only helper lifetime and fail-closed tethering remain under investigation; do not assume they work after Wi-Fi disconnect or reboot.
- Service checks distinguish a reachable route from confirmed messages, media and calls. A TLS handshake does not prove video playback.
- In-app updates check public GitHub Releases daily when Argus opens, with a manual check in the app. Downloads require user approval. SHA-256, package, version and the installed signing certificate are checked before Android's installation confirmation.

## Compatibility

ARM64 Android 11/API 30 and later. LIGHT and hotspot have separate capability requirements. Samsung, Xiaomi, Honor and other brands are not whitelisted; hidden tethering APIs, debugging permissions, firmware and battery management can still prevent protected hotspot. Android 11/12 compatibility is a build/lint target pending hardware testing, not a promise of protected tethering.

## Build

JDK 17, Go 1.26.3, Android SDK 36, build-tools 36.0.0 and NDK 28.2.13676358.

```sh
go install golang.org/x/mobile/cmd/gomobile@v0.0.0-20260803200217-62cee1672c8e
gomobile init
./gradlew :app:assembleDebug :app:lintDebug
(cd datapath && go test -race ./...)
bash scripts/verify-apk.sh app/build/outputs/apk/debug/app-debug.apk
```

CI builds are verification artifacts, not upgrades for an installed alpha: GitHub's ephemeral debug key differs from the distribution key. Published alpha APKs are signed locally with the existing alpha certificate so existing installations can update. Private signing keys and device diagnostics never belong in this repository. Keep the distribution keystore backed up privately; replacing it prevents upgrades.

## YouTube adaptation

See [the porting notes](docs/youtube-adaptation.md). Strategies learned for a router are not copied blindly to Android. Existing RVX client patches are separate from this project; no patched YouTube APK is distributed here.

## Updates and publication

Release assets: `Argus-Connect-alpha.apk`, `update.json`, `SHA256.txt`. Use `scripts/prepare-release.py` to generate metadata from a locally signed build. The updater consumes `releases/latest`; each alpha release intended for this channel is published as the latest non-draft release, with alpha clearly stated in its title. Signed updates are interactive, never silently installed.

Network recovery does not hide traffic metadata from the carrier or optional proxy operators. Optional service proxy endpoints are opt-in. Argus does not collect message contents or call audio.

Apache-2.0; see [third-party notices](THIRD_PARTY_NOTICES.md).
