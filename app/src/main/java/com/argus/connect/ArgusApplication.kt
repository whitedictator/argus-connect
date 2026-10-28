package com.argus.connect

import android.app.Application
import com.flyfishxu.kadb.cert.KadbCert
import com.flyfishxu.kadb.cert.OkioFilePrivateKeyStore
import okio.Path.Companion.toPath

class ArgusApplication : Application() {
    lateinit var bridge: BridgeController
        private set
    lateinit var light: LightController
        private set

    override fun onCreate() {
        super.onCreate()
        DiagnosticJournal.initialize(this)
        KadbCert.configure(
            OkioFilePrivateKeyStore(filesDir.resolve("adb/argus_adb_key.pem").absolutePath.toPath()),
        )
        bridge = BridgeController(this)
        light = LightController(this)
    }
}
