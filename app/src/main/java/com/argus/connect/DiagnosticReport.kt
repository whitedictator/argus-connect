package com.argus.connect

import android.content.Context
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.os.Build
import java.io.File
import java.time.Instant

object DiagnosticReport {
    fun create(context: Context, light: LightState, bridge: BridgeState): File {
        val directory = File(context.cacheDir, "diagnostics").apply { mkdirs() }
        val report = File(directory, "argus-diagnostic-${System.currentTimeMillis()}.txt")
        val connectivity = context.getSystemService(ConnectivityManager::class.java)
        val network = connectivity.activeNetwork
        val capabilities = network?.let(connectivity::getNetworkCapabilities)
        val link = network?.let(connectivity::getLinkProperties)
        val rawPolicy = (context.applicationContext as ArgusApplication).light.nativeStatus()

        report.writeText(
            buildString {
                appendLine("ARGUS CONNECT DIAGNOSTIC")
                appendLine("generated=${Instant.now()}")
                appendLine("app=${BuildConfig.VERSION_NAME} (${BuildConfig.VERSION_CODE})")
                appendLine("device=${Build.MANUFACTURER} ${Build.MODEL}")
                appendLine("android=${Build.VERSION.RELEASE} sdk=${Build.VERSION.SDK_INT}")
                appendLine("securityPatch=${Build.VERSION.SECURITY_PATCH}")
                appendLine()
                appendLine("[CURRENT NETWORK]")
                appendLine("transports=${transportNames(capabilities).joinToString(",").ifEmpty { "none" }}")
                appendLine("validated=${capabilities?.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED) == true}")
                appendLine("captivePortal=${capabilities?.hasCapability(NetworkCapabilities.NET_CAPABILITY_CAPTIVE_PORTAL) == true}")
                appendLine("metered=${connectivity.isActiveNetworkMetered}")
                appendLine("mtu=${link?.mtu ?: 0}")
                appendLine("privateDns=${link?.isPrivateDnsActive == true}")
                appendLine("dnsServerCount=${link?.dnsServers?.size ?: 0}")
                appendLine()
                appendLine("[VISIBLE STATE]")
                appendLine("lightPhase=${light.phase}")
                appendLine("lightTitle=${light.title}")
                appendLine("lightDetail=${light.detail}")
                appendLine("dnsReady=${light.dohReady} dnsTransport=${light.dnsTransport}")
                appendLine("telegram=${light.telegram}")
                appendLine("whatsapp=${light.whatsapp}")
                appendLine("whatsappMedia=${light.whatsappMedia}")
                appendLine("signal=${light.signal}")
                appendLine("threema=${light.threema}")
                appendLine("youtube=${light.youtube}")
                appendLine("youtubeFeed=${light.youtubeFeed}")
                appendLine("youtubeThumbnails=${light.youtubeThumbnails}")
                appendLine("youtubeVideo=${light.youtubeVideo}")
                appendLine("automaticChecksComplete=${light.automaticChecksComplete}")
                appendLine("probeAttempts=${light.probeAttempts.toSortedMap()}")
                appendLine("currentStrategies=${light.currentStrategies.toSortedMap()}")
                appendLine("currentRoutes=${light.currentRoutes.toSortedMap()}")
                appendLine("throughputKbps=${light.throughputKbps.toSortedMap()}")
                appendLine("failureReasons=${light.failureReasons.toSortedMap()}")
                appendLine("bridgePhase=${bridge.phase}")
                appendLine("bridgeTitle=${DiagnosticJournal.redact(bridge.title)}")
                appendLine("bridgeDetail=${DiagnosticJournal.redact(bridge.detail)}")
                appendLine("bridgePaired=${(context.applicationContext as ArgusApplication).bridge.hasSavedPairing()}")
                appendLine("bridgeInterfacePresent=${!bridge.interfaceName.isNullOrBlank()}")
                appendLine()
                appendLine("[NATIVE AGGREGATES - PUBLIC SERVICE ENDPOINTS ONLY; NO PAYLOADS]")
                appendLine(rawPolicy)
                appendLine()
                appendLine("[SESSION TIMELINE]")
                DiagnosticJournal.snapshot().forEach(::appendLine)
                appendLine()
                appendLine("Privacy: no message content, contacts, account IDs, device IP, DNS names, or packet payloads are collected.")
            },
        )
        return report
    }

    private fun transportNames(capabilities: NetworkCapabilities?): List<String> = buildList {
        if (capabilities == null) return@buildList
        if (capabilities.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)) add("wifi")
        if (capabilities.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR)) add("cellular")
        if (capabilities.hasTransport(NetworkCapabilities.TRANSPORT_VPN)) add("vpn")
        if (capabilities.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)) add("ethernet")
    }
}
