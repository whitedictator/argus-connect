package com.argus.connect

import android.content.Context
import android.graphics.Bitmap
import android.graphics.Canvas
import android.graphics.Color
import android.widget.RemoteViews
import java.util.concurrent.ConcurrentHashMap

internal object ServiceNotification {
    private val icons = ConcurrentHashMap<String, Bitmap>()
    private fun label(r: CheckResult) = when(r) {
        CheckResult.WORKS -> "тест пройден"; CheckResult.ROUTE -> "маршрут найден, работа приложения не подтверждена"
        CheckResult.FAILED -> "тест не пройден"; CheckResult.FALLBACK -> "частичный результат"; CheckResult.WAITING -> "пока не проверено"
    }
    private fun aggregate(vararg results: CheckResult): CheckResult = when {
        results.all { it == CheckResult.WORKS } -> CheckResult.WORKS
        results.any { it == CheckResult.FAILED } && results.none { it == CheckResult.WORKS || it == CheckResult.ROUTE } -> CheckResult.FAILED
        else -> CheckResult.ROUTE
    }
    fun summary(s: LightState) = if (s.automaticChecksComplete) "Проверки завершены · подробности в Argus" else "Проверяю доступность сервисов"
    fun signature(s: LightState) = listOf(s.phase,s.automaticChecksComplete,s.telegram,s.youtubeFeed,s.youtubeThumbnails,s.youtubeVideo,s.whatsapp,s.whatsappMedia,s.signal,s.threema,s.facebook,s.instagram,s.twitter).joinToString()

    fun views(context: Context, s: LightState, expanded: Boolean): RemoteViews {
        val root = RemoteViews(context.packageName, if(expanded) R.layout.notification_services_expanded else R.layout.notification_services)
        fun add(row: Int, key: String, name: String, result: CheckResult) {
            val cell = RemoteViews(context.packageName,R.layout.notification_service_cell)
            val icon = icons.getOrPut(key) {
                Bitmap.createBitmap(64,64,Bitmap.Config.ARGB_8888).also { bitmap ->
                    ServiceGlyph(context,key).apply { layout(0,0,64,64) }.draw(Canvas(bitmap))
                }
            }
            cell.setImageViewBitmap(R.id.service_icon,icon)
            cell.setTextViewText(R.id.service_label,name)
            cell.setViewVisibility(R.id.service_label, android.view.View.GONE)
            cell.setImageViewResource(R.id.service_mark, when(result) {
                CheckResult.WORKS -> R.drawable.ic_status_green
                CheckResult.FAILED -> R.drawable.ic_status_red
                else -> R.drawable.ic_status_amber
            })
            cell.setContentDescription(R.id.service_cell,"$name: ${label(result)}")
            root.addView(row,cell)
        }
        add(R.id.notification_services,"telegram","TG",s.telegram.messages)
        add(R.id.notification_services,"youtube","YouTube",aggregate(s.youtubeFeed,s.youtubeThumbnails,s.youtubeVideo))
        add(R.id.notification_services,"whatsapp","WA",s.whatsapp.messages)
        if(expanded) {
            add(R.id.notification_services,"signal","SG",s.signal.messages)
            add(R.id.notification_social,"facebook","Facebook",aggregate(s.facebook.page,s.facebook.media))
            add(R.id.notification_social,"instagram","Instagram",aggregate(s.instagram.page,s.instagram.media))
            add(R.id.notification_social,"twitter","X",aggregate(s.twitter.page,s.twitter.media))
            add(R.id.notification_social,"threema","TH",s.threema.messages)
            root.setTextViewText(R.id.notification_details,
                "YouTube: видео — ${label(s.youtubeVideo)}\n"+
                "WhatsApp: медиа — ${label(s.whatsappMedia)}\n"+
                "Ответ сервера не подтверждает доставку сообщений и звонки.")
        }
        return root
    }
}
