package com.argus.connect

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.wifi.WifiInfo
import android.telephony.TelephonyManager
import java.security.MessageDigest

/** Small, expiring cache of aggregate tactics, scoped to an identifiable network. */
internal class NetworkPolicyMemory(private val context: Context) {
    private val prefs = context.getSharedPreferences("argus_network_tactics_v1", Context.MODE_PRIVATE)

    fun key(network: Network): String? = runCatching {
        val manager = context.getSystemService(ConnectivityManager::class.java)
        val caps = manager.getNetworkCapabilities(network) ?: return null
        val link = manager.getLinkProperties(network) ?: return null
        val identity = when {
            caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> {
                val operator = context.getSystemService(TelephonyManager::class.java).networkOperator
                if (operator.isNullOrBlank()) return null
                "cellular:$operator"
            }
            caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> {
                val ssid = (caps.transportInfo as? WifiInfo)?.ssid
                if (ssid.isNullOrBlank() || ssid == "<unknown ssid>") return null
                "wifi:$ssid"
            }
            else -> return null
        }
        val material = listOf(identity, link.dnsServers.mapNotNull { it.hostAddress }.sorted().joinToString(), link.nat64Prefix, link.isPrivateDnsActive, link.mtu).joinToString("|")
        MessageDigest.getInstance("SHA-256").digest(material.toByteArray()).joinToString("") { "%02x".format(it) }
    }.getOrNull()

    fun load(key: String?): String {
        if (key == null) return ""
        val age = System.currentTimeMillis() - prefs.getLong("$key.time", 0)
        if (age !in 0..MAX_AGE) return ""
        return prefs.getString("$key.data", "").orEmpty()
    }

    fun save(key: String?, value: String) {
        if (key == null || value.isBlank() || value == prefs.getString("$key.data", "")) return
        val editor = prefs.edit().putString("$key.data", value).putLong("$key.time", System.currentTimeMillis())
        prefs.all.keys.filter { it.endsWith(".time") && it != "$key.time" }
            .sortedByDescending { prefs.getLong(it, 0) }.drop(7).forEach {
                editor.remove(it).remove(it.removeSuffix(".time") + ".data")
            }
        editor.apply()
    }

    private companion object { const val MAX_AGE = 7L * 24 * 60 * 60 * 1000 }
}
