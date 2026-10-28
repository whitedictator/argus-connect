#!/usr/bin/env bash
set -euo pipefail

apk="${1:?APK path required}"
test -f "$apk"

aapt_bin="${ANDROID_HOME:?ANDROID_HOME required}/build-tools/36.0.0/aapt"
badging="$($aapt_bin dump badging "$apk")"

version_code="$(sed -n 's/^[[:space:]]*versionCode = \([0-9]*\).*/\1/p' app/build.gradle.kts)"
version_name="$(sed -n 's/^[[:space:]]*versionName = "\([^"]*\)".*/\1/p' app/build.gradle.kts)"
test -n "$version_code" && test -n "$version_name"
grep -Fq "package: name='com.argus.connect.alpha.debug' versionCode='$version_code' versionName='$version_name-debug'" <<<"$badging"
grep -Fq "application-label:'Argus Connect α'" <<<"$badging"
grep -Fq "native-code: 'arm64-v8a'" <<<"$badging"

grep -Rqs --exclude='*_test.go' "object BridgeMain" app/src/main/java/com/argus/connect/bridge
grep -Rqs --exclude='*_test.go' "setPreferTestNetworks" app/src/main/java/com/argus/connect/bridge
grep -Rqs --exclude='*_test.go' "android_setsocknetwork" datapath
grep -Rqs --exclude='*_test.go' "class ArgusLightService : VpnService" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "strategyTLSRecord" datapath
grep -Rqs --exclude='*_test.go' "strategyTLSMulti" datapath
grep -Rqs --exclude='*_test.go' "strategyTLSCombo" datapath
grep -Rqs --exclude='*_test.go' "strategyComboFast" datapath
grep -Rqs --exclude='*_test.go' "strategyComboMid" datapath
grep -Rqs --exclude='*_test.go' "strategyOOB" datapath
grep -Rqs --exclude='*_test.go' "strategyDisorder" datapath
grep -Rqs --exclude='*_test.go' "strategyTLSEmpty" datapath
grep -Rqs --exclude='*_test.go' "strategySmallMSS" datapath
grep -Rqs --exclude='*_test.go' "serviceSignal" datapath
grep -Rqs --exclude='*_test.go' "serviceThreema" datapath
grep -Rqs --exclude='*_test.go' "newDoHResolver" datapath
grep -Rqs --exclude='*_test.go' "resolveDoT" datapath
grep -Rqs --exclude='*_test.go' "UDP fallback" datapath
grep -Rqs --exclude='*_test.go' "SetNAT64Prefix" datapath
grep -Rqs --exclude='*_test.go' "SetProxyPool" datapath
grep -Rqs --exclude='*_test.go' "launchResident" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "HELPER_VERSION = 9" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "class BridgeBinderHost" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "Looper.prepareMainLooper" app/src/main/java/com/argus/connect/bridge
grep -Rqs --exclude='*_test.go' "argus_network_tactics_v1" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "youtubei/v1/browse" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "serverBytes >= 32\*1024" datapath/forward.go
grep -Rqs --exclude='*_test.go' "AUTOTEST_DEADLINE_MS = 45_000L" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "telegram-wss-dc" datapath
grep -Rqs --exclude='*_test.go' "Sec-WebSocket-Protocol" datapath
grep -Rqs --exclude='*_test.go' "probeTelegramObfuscated" datapath
grep -Rqs --exclude='*_test.go' "ARGUS_SELF_TEST_OK" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "AdbShellPacket.Exit" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "export CLASSPATH=" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "openShell(command)" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "setsid" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "NewProtocolCUBIC" datapath
grep -Rqs --exclude='*_test.go' "alternateAddress" datapath
grep -Rqs --exclude='*_test.go' "result.Fallback" datapath/policy.go
grep -Rqs --exclude='*_test.go' "setExemptFromEntitlementCheck" app/src/main/java/com/argus/connect/bridge
grep -Rqs --exclude='*_test.go' "onUpstreamChanged" app/src/main/java/com/argus/connect/bridge

xmltree="$($aapt_bin dump xmltree "$apk" AndroidManifest.xml)"
grep -Fq "android.net.VpnService" <<<"$xmltree"
grep -Fq "android.service.quicksettings.action.QS_TILE_PREFERENCES" <<<"$xmltree"
grep -Fq "android.permission.BIND_VPN_SERVICE" <<<"$xmltree"

verify_dir="$(mktemp -d)"
trap 'rm -rf "$verify_dir"' EXIT
unzip -p "$apk" lib/arm64-v8a/libgojni.so > "$verify_dir/libgojni.so"
: > "$verify_dir/classes.txt"
while IFS= read -r dex; do
    unzip -p "$apk" "$dex" | strings >> "$verify_dir/classes.txt"
done < <(unzip -Z1 "$apk" 'classes*.dex')
strings "$verify_dir/libgojni.so" > "$verify_dir/native.txt"
grep -Fq "ArgusLightService" "$verify_dir/classes.txt"
grep -Fq "RepairDeckView" "$verify_dir/classes.txt"
grep -Fq "UtilityActionView" "$verify_dir/classes.txt"
grep -Fq "ServiceNotification" "$verify_dir/classes.txt"
grep -Fq "BridgeBinderProvider" "$verify_dir/classes.txt"
grep -Fq "SetMTProxyDiscovery" "$verify_dir/native.txt"
grep -Fq "unwrapLocalMTProto" "$verify_dir/native.txt"
grep -Fq "tls-record-split" "$verify_dir/native.txt"
grep -Fq "tls-multi-record" "$verify_dir/native.txt"
grep -Fq "tls-record-sni-combo" "$verify_dir/native.txt"
grep -Fq "tls-record-sni-combo-fast" "$verify_dir/native.txt"
grep -Fq "tls-record-sni-combo-balanced" "$verify_dir/native.txt"
grep -Fq "tcp-urgent-probe" "$verify_dir/native.txt"
grep -Fq "tcp-lowttl-disorder" "$verify_dir/native.txt"
grep -Fq "tls-empty-record-prefix" "$verify_dir/native.txt"
grep -Fq "tcp-small-mss" "$verify_dir/native.txt"
grep -Fq "cloudflare-dns.com" "$verify_dir/native.txt"
grep -Fq "dns.quad9.net" "$verify_dir/native.txt"
grep -Fq "dns.adguard-dns.com" "$verify_dir/native.txt"
grep -Fq "DoT handshake" "$verify_dir/native.txt"
grep -Fq "UDP fallback" "$verify_dir/native.txt"
grep -Fq "nat64-ipv6" "$verify_dir/native.txt"
grep -Fq "socks5-" "$verify_dir/native.txt"
grep -Fq "telegramAlternates" "$verify_dir/native.txt"
grep -Fq "ProbeTelegramNetwork" "$verify_dir/native.txt"
grep -Fq "2001:67c:4e8:f002::a" "$verify_dir/native.txt"
grep -Fq "strategyKbps" "$verify_dir/native.txt"
grep -Fq "HTTP2-Chromium" "$verify_dir/native.txt"
grep -Fq "signal-official-reflector/" "$verify_dir/native.txt"
grep -Fq "X-Signal-Host" "$verify_dir/native.txt"
grep -Fq "routeKbps" "$verify_dir/native.txt"
grep -Fq "telegram-wss-dc" "$verify_dir/native.txt"
grep -Fq "Sec-WebSocket-Protocol" "$verify_dir/native.txt"
grep -Fq "chat.signal.org" "$verify_dir/classes.txt"
grep -Fq "ds-apip.threema.ch" "$verify_dir/classes.txt"
grep -Fq "redirector.googlevideo.com" "$verify_dir/classes.txt"
grep -Fq "static.whatsapp.net" "$verify_dir/classes.txt"
grep -Fq "mmg.whatsapp.net" "$verify_dir/classes.txt"
grep -Fq "jNQXAC9IVRw" "$verify_dir/classes.txt"
grep -Fq "ARGUS_SELF_TEST_OK" "$verify_dir/classes.txt"
grep -Fq "youtubeVideo" "$verify_dir/classes.txt"
grep -Fq "whatsappMedia" "$verify_dir/classes.txt"
grep -Fq "setExemptFromEntitlementCheck" "$verify_dir/classes.txt"
grep -Fq "onUpstreamChanged" "$verify_dir/classes.txt"
grep -Fq "startTethering failed after 3 attempts" "$verify_dir/classes.txt"
grep -Fq "existing Android pairing recovered" "$verify_dir/classes.txt"
grep -Fq "resident helper did not start" "$verify_dir/classes.txt"
grep -Fq "argus-bridge-guardian" "$verify_dir/classes.txt"
grep -Fq "ADB shell authenticated as uid 2000 transport=" "$verify_dir/classes.txt"
grep -Fq "loopback" "$verify_dir/classes.txt"
grep -Fq "argus-adb-discovery" "$verify_dir/classes.txt"
grep -Rqs --exclude='*_test.go' "ВОССТАНОВИТЬ BRIDGE БЕЗ КОДА" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "Анализирую текущую сеть" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "нужен тестовый звонок" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "ОТПРАВИТЬ ТЕХНИЧЕСКИЙ ОТЧЁТ" app/src/main/java/com/argus/connect
grep -Rqs --exclude='*_test.go' "_adb-tls-pairing._tcp" app/src/main/java/com/argus/connect
grep -Fq "androidx.core.content.FileProvider" <<<"$xmltree"

cat > BUILD-PROOF.txt <<EOF
Argus Connect $version_name
scope=artifact-identity-and-code-presence-only; not proof of carrier bypass or leak-free hotspot
package=com.argus.connect.alpha.debug
minSdk=30
abi=arm64-v8a
light=local-VpnService+gVisor-netstack+physical-network-binding
diagnostics=real-mtproto-nonce+per-component-media-probes+explicit-test-completion+redacted-native-route-status
services=telegram+whatsapp+signal+threema+youtube
strategies=direct-baseline+tls-record-split+tls-multi-record+tls-record-sni-combo-fast+tls-record-sni-combo-balanced+tls-record-sni-combo-strong+tls-empty-record-prefix+sni-label-split+tcp-microburst+tcp-small-mss+tcp-urgent-probe+tcp-lowttl-disorder+mtproto-stagger+adaptive-quic-fallback+multi-address-ipv4-ipv6-race+telegram-dc-ipv4-ipv6-and-official-port-race+official-telegram-wss-obfuscated-mtproto+android-nat64-race+generic-socks5-tcp-rotation+cubic
dns=per-query-preferred-DoH+parallel-DoH+DoT-cloudflare+google+quad9+adguard-over-ipv4+ipv6+bounded-discovery-circuit-breaker+reported-udp-fallback
light_fail_closed=selected-physical-network-only+kernel-bind-check+network-loss-tun-close
bridge=binder-v9-shell-uid-provider+descriptor-transfer+owner-death-cleanup+test-network-upstream-check
telegram=opt-in-FakeTLS-MTProxy+transparent-EF-EE-DD-transport-adapter+quick-ack-conversion+nonce-verified-pool+bounded-opt-in-public-discovery
support=privacy-redacted-on-device-diagnostic+android-share-sheet
upstream=TestNetworkManager+setPreferTestNetworks
datapath=gVisor-netstack+android_setsocknetwork
protection=vpn-handle-kernel-check+sole-tether-upstream-check+vpn-loss-stop; OS fallback race not excluded
shizuku=not-required
EOF
