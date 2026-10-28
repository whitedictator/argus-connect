package com.argus.connect

import android.app.PendingIntent
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.net.VpnService
import android.graphics.drawable.Icon
import android.service.quicksettings.Tile
import android.service.quicksettings.TileService

class ArgusTileService : TileService() {
    private val light get() = (application as ArgusApplication).light
    private var observation: AutoCloseable? = null

    override fun onStartListening() {
        super.onStartListening()
        observation?.close()
        observation = light.observe { mainExecutor.execute { render() } }
        render()
    }

    override fun onStopListening() {
        observation?.close()
        observation = null
        super.onStopListening()
    }

    override fun onClick() {
        super.onClick()
        if (isLocked) { unlockAndRun { toggleLight() } } else toggleLight()
    }

    private fun toggleLight() {
        val state = light.current()
        val hotspotActive = (application as ArgusApplication).bridge.current().isActive
        if (!state.isRunning && !state.isBusy && !hotspotActive && VpnService.prepare(this) == null) {
            light.start()
        } else if (!state.isRunning && !state.isBusy) {
            val intent = Intent(this, MainActivity::class.java)
                .setAction(ACTION_ENABLE_LIGHT)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP)
            if (android.os.Build.VERSION.SDK_INT >= 34) {
                startActivityAndCollapse(PendingIntent.getActivity(this, 2, intent,
                    PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT))
            } else {
                openLegacyTile(intent)
            }
        } else {
            light.stop("Выключено из быстрых настроек")
        }
        render()
    }

    // Android 13 has only the Intent overload; Android 14+ uses PendingIntent above.
    @android.annotation.SuppressLint("StartActivityAndCollapseDeprecated")
    @Suppress("DEPRECATION")
    private fun openLegacyTile(intent: Intent) { startActivityAndCollapse(intent) }

    private fun render() {
        val state = light.current()
        qsTile?.apply {
            this.state = when {
                state.isRunning || state.isBusy -> Tile.STATE_ACTIVE
                else -> Tile.STATE_INACTIVE
            }
            label = "Argus LIGHT"
            icon = Icon.createWithResource(this@ArgusTileService, R.drawable.ic_argus_tile)
            subtitle = when (state.phase) {
                LightPhase.ANALYZING -> "Анализ сети"
                LightPhase.BASELINE -> "Готов"
                LightPhase.ACTIVE -> "Работает"
                LightPhase.VERIFYING -> "Проверяет"
                LightPhase.LIMITED -> "Ограничено"
                LightPhase.ERROR -> "Требует внимания"
                else -> "Выключен"
            }
            updateTile()
        }
    }

    companion object {
        const val ACTION_ENABLE_LIGHT = "com.argus.connect.ENABLE_LIGHT_FROM_TILE"
        fun refresh(context: Context) {
            requestListeningState(context, ComponentName(context, ArgusTileService::class.java))
        }
    }
}
