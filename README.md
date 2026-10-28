# Argus Connect

A connection assistant for Android. Argus helps check popular messaging apps and online services, diagnose connectivity issues, and try suitable fixes.

- Service-by-service connection checks and clear status information.
- Adaptive network recovery with priority for previously successful settings.
- Protected internet sharing through an active VPN, with LIGHT switched off if preferred.
- In-app update checks and verified APK downloads from GitHub Releases.

**Experimental alpha · Android 11+ · ARM64.** Available features depend on the Android version, firmware and permissions. Some features require initial setup. A successful endpoint check does not confirm that messaging, media or calls all work.

## Install and update

Download the APK from [Releases](https://github.com/whitedictator/argus-connect/releases).
Argus checks for updates when opened, at most once a day. Tap the version row for a manual check. Downloads require user approval; the app checks SHA-256, package, version and signing certificate before Android asks to confirm installation.

For protected sharing through another VPN, the Argus bridge service stays in the background while LIGHT is off. Losing the selected VPN stops protected sharing. Restarting the phone or force-stopping Argus may require bridge recovery.

## Build

JDK 17, Go 1.26.3, Android SDK 36, build-tools 36.0.0 and NDK 28.2.13676358.

```sh
go install golang.org/x/mobile/cmd/gomobile@v0.0.0-20260803200217-62cee1672c8e
gomobile init
./gradlew :app:assembleDebug :app:lintDebug
(cd datapath && go test -race ./...)
bash scripts/verify-apk.sh app/build/outputs/apk/debug/app-debug.apk
```

CI artifacts verify the source build. Published APK upgrades are signed locally with the existing alpha certificate; private signing keys are not stored in this repository. `scripts/prepare-release.py` creates the update metadata and checksum.

## License

Apache-2.0. See [LICENSE](LICENSE) and [third-party notices](THIRD_PARTY_NOTICES.md).
