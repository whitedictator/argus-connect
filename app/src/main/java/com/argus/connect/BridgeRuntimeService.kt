package com.argus.connect

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.IBinder

class BridgeRuntimeService : Service() {
    override fun onCreate() {
        super.onCreate()
        current = this
        val manager = getSystemService(NotificationManager::class.java)
        manager.createNotificationChannel(
            NotificationChannel(CHANNEL, "Защищённый hotspot", NotificationManager.IMPORTANCE_LOW),
        )
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            (application as ArgusApplication).bridge.stop("Остановлено из уведомления")
            stopSelf()
            return START_NOT_STICKY
        }
        startForeground(NOTIFICATION_ID, notification())
        // The foreground service is also the lifetime owner of the raw local
        // ADB stream guarding BridgeMain. If Android reclaims the app process,
        // ArgusApplication restores the saved pairing and guardian on restart.
        return START_STICKY
    }

    override fun onDestroy() {
        if (current === this) current = null
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun notification(): Notification {
        val state = (application as ArgusApplication).bridge.current()
        val active = state.isActive
        val open = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val stop = PendingIntent.getService(
            this,
            1,
            Intent(this, BridgeRuntimeService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        return Notification.Builder(this, CHANNEL)
            .setSmallIcon(R.drawable.ic_argus_status)
            .setContentTitle(if (active) "Argus: hotspot защищён" else state.title)
            .setContentText(state.detail)
            .setContentIntent(open)
            .setOngoing(true)
            .addAction(Notification.Action.Builder(null, "Выключить", stop).build())
            .build()
    }

    companion object {
        @Volatile private var current: BridgeRuntimeService? = null
        fun refresh(context: Context) {
            current?.let { service ->
                context.getSystemService(NotificationManager::class.java)
                    .notify(NOTIFICATION_ID, service.notification())
            }
        }
        private const val CHANNEL = "argus_bridge"
        private const val NOTIFICATION_ID = 4101
        private const val ACTION_STOP = "com.argus.connect.STOP"

        fun start(context: Context) {
            context.startForegroundService(Intent(context, BridgeRuntimeService::class.java))
        }

        fun stop(context: Context) {
            context.stopService(Intent(context, BridgeRuntimeService::class.java))
        }
    }
}
