package com.argus.connect

import android.content.Context
import java.net.HttpURLConnection
import java.security.KeyStore
import java.security.cert.CertificateFactory
import javax.net.ssl.HttpsURLConnection
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLSocketFactory
import javax.net.ssl.TrustManagerFactory

/** Signal's published CA, scoped to its chat probe. Never a global TLS override.
 * Source: signalapp/Signal-Desktop config/default.json, retrieved 2026-09-08.
 * Certificate chain, expiry and standard hostname verification stay enabled.
 */
internal object SignalTrust {
    @Volatile private var factory: SSLSocketFactory? = null

    fun configure(context: Context, connection: HttpURLConnection) {
        if (connection !is HttpsURLConnection || connection.url.host != "chat.signal.org") return
        val pinned = factory ?: synchronized(this) {
            factory ?: run {
                val ca = context.resources.openRawResource(R.raw.signal_ca).use {
                    CertificateFactory.getInstance("X.509").generateCertificate(it)
                }
                val store = KeyStore.getInstance(KeyStore.getDefaultType()).apply {
                    load(null, null)
                    setCertificateEntry("signal-published-root", ca)
                }
                val trust = TrustManagerFactory.getInstance(TrustManagerFactory.getDefaultAlgorithm()).apply { init(store) }
                SSLContext.getInstance("TLS").apply { init(null, trust.trustManagers, null) }.socketFactory.also { factory = it }
            }
        }
        connection.sslSocketFactory = pinned
    }
}
