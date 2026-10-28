package com.argus.connect

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/** Debug builds only; shell DUMP permission required. No traffic content exported. */
class DebugDiagnosticsReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val app = context.applicationContext as ArgusApplication
        val pending = goAsync()
        Thread {
            try {
                when (intent.getStringExtra("operation")) {
                    "voice-mapping-trial" -> context.startService(
                        Intent(context, ArgusLightService::class.java)
                            .setAction(ArgusLightService.ACTION_VOICE_MAPPING_TRIAL)
                            .putExtra("enabled", intent.getBooleanExtra("enabled", false)),
                    )
                    "signal-direct-trial" -> context.startService(
                        Intent(context, ArgusLightService::class.java)
                            .setAction(ArgusLightService.ACTION_SIGNAL_DIRECT_TRIAL)
                            .putExtra("enabled", intent.getBooleanExtra("enabled", false)),
                    )
                    "signal-relay-config" -> {
                        context.getSharedPreferences("argus_light", Context.MODE_PRIVATE).edit()
                            .putBoolean("signal_relay_sgpx_consent", intent.getBooleanExtra("enabled", false)).commit()
                        app.light.stop("Изменён маршрут Signal; включите LIGHT")
                    }
                    "pair-bridge" -> app.bridge.pair(intent.getStringExtra("code").orEmpty())
                    "proxy-config" -> app.light.saveProxyPool(intent.getStringExtra("pool").orEmpty(), intent.getBooleanExtra("discover", false))
                    "hotspot" -> app.bridge.start()
                    "stop-hotspot" -> app.bridge.stop("Остановлено пользователем")
                    "light" -> app.light.start()
                    "analysis" -> app.light.analyzeCurrentNetwork()
                    "stop-light" -> app.light.stop("Остановлено пользователем")
                }
                val report = DiagnosticReport.create(context, app.light.current(), app.bridge.current())
                report.copyTo(context.cacheDir.resolve("diagnostics/usb-latest.txt"), overwrite = true)
            } finally {
                pending.finish()
            }
        }.start()
    }
}
