package com.argus.connect

import android.app.Activity
import android.app.AlertDialog
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.provider.Settings
import androidx.core.content.FileProvider
import org.json.JSONObject
import java.io.File
import java.net.HttpURLConnection
import java.net.URL
import java.security.MessageDigest
import java.util.concurrent.Executors

/** Public GitHub metadata only. APKs must match this installed package and signer. */
class AppUpdates(private val activity: Activity, private val status: (String) -> Unit) {
    private val worker = Executors.newSingleThreadExecutor()
    private val prefs = activity.getSharedPreferences("app_updates", Context.MODE_PRIVATE)
    private val repository = "whitedictator/argus-connect"
    private val maxApkBytes = 100L * 1024 * 1024
    private var pendingInstall: File? = null
    @Volatile private var busy = false
    private data class Release(val code: Long, val version: String, val url: String, val sha256: String, val size: Long)

    fun check(manual: Boolean = false) {
        if (busy) return
        if (!manual && System.currentTimeMillis() - prefs.getLong("checked_at", 0) < 24 * 60 * 60 * 1000L) return
        busy = true
        if (manual) status("Проверяю обновления…")
        worker.execute {
            runCatching {
                val release = JSONObject(get("https://api.github.com/repos/$repository/releases/latest", 256 * 1024).toString(Charsets.UTF_8))
                check(!release.optBoolean("draft")) { "Релиз ещё не опубликован" }
                val assets = release.getJSONArray("assets")
                val manifest = (0 until assets.length()).map { assets.getJSONObject(it) }.single { it.optString("name") == "update.json" }
                val data = JSONObject(get(trustedAsset(manifest.getString("browser_download_url")), 16 * 1024).toString(Charsets.UTF_8))
                check(data.getString("applicationId") == activity.packageName) { "Релиз для другого приложения" }
                check(data.getInt("minSdk") <= android.os.Build.VERSION.SDK_INT) { "Требуется более новая версия Android" }
                val assetName = data.getString("asset")
                val apk = (0 until assets.length()).map { assets.getJSONObject(it) }.single { it.optString("name") == assetName }
                val size = apk.getLong("size")
                check(size in 1..maxApkBytes) { "Некорректный размер APK" }
                val sha = data.getString("sha256").lowercase()
                check(sha.matches(Regex("[a-f0-9]{64}"))) { "В релизе нет корректного SHA‑256" }
                val digest = apk.optString("digest")
                check(digest.isEmpty() || digest == "sha256:$sha") { "Хеши GitHub и манифеста не совпадают" }
                prefs.edit().putLong("checked_at", System.currentTimeMillis()).apply()
                Release(data.getLong("versionCode"), data.getString("versionName"), trustedAsset(apk.getString("browser_download_url")), sha, size)
            }.onSuccess { release -> activity.runOnUiThread {
                if (activity.isFinishing || activity.isDestroyed) return@runOnUiThread
                if (release.code <= BuildConfig.VERSION_CODE) { status("Версия ${BuildConfig.VERSION_NAME} · обновлений нет"); return@runOnUiThread }
                status("Доступна версия ${release.version}")
                AlertDialog.Builder(activity).setTitle("Argus ${release.version}")
                    .setMessage("Скачать ${release.size / (1024 * 1024)} МБ с GitHub? Перед установкой будут проверены хеш, пакет и подпись APK.")
                    .setNegativeButton("Позже", null).setPositiveButton("Скачать") { _, _ -> download(release) }.show()
            }}.onFailure { failure -> activity.runOnUiThread { if (manual) status("Обновления: ${failure.message?.take(160) ?: "нет соединения"}") } }
            busy = false
        }
    }

    private fun download(release: Release) {
        if (busy) return
        busy = true
        status("Скачиваю ${release.version}…")
        worker.execute {
            val directory = File(activity.cacheDir, "updates").apply { mkdirs() }
            val partial = File(directory, "download.part")
            val target = File(directory, "argus-update.apk")
            runCatching {
                val digest = MessageDigest.getInstance("SHA-256")
                val connection = open(release.url)
                var total = 0L
                try {
                    connection.inputStream.use { input -> partial.outputStream().use { output ->
                        val buffer = ByteArray(64 * 1024)
                        while (true) {
                            val n = input.read(buffer); if (n < 0) break
                            total += n
                            check(total <= release.size && total <= maxApkBytes) { "Размер скачанного APK превышен" }
                            digest.update(buffer, 0, n); output.write(buffer, 0, n)
                        }
                    }}
                } finally { connection.disconnect() }
                check(total == release.size) { "APK скачан не полностью" }
                check(digest.digest().joinToString("") { "%02x".format(it) } == release.sha256) { "Хеш APK не совпал" }
                verifyArchive(partial, release.code)
                target.delete(); check(partial.renameTo(target)) { "Не удалось сохранить APK" }
                target
            }.onSuccess { file -> activity.runOnUiThread { pendingInstall = file; offerInstall() } }
                .onFailure { failure -> partial.delete(); activity.runOnUiThread { status("Установка отменена: ${failure.message?.take(180)}") } }
            busy = false
        }
    }

    private fun verifyArchive(file: File, code: Long) {
        val pm = activity.packageManager
        @Suppress("DEPRECATION") val candidate = pm.getPackageArchiveInfo(file.absolutePath, PackageManager.GET_SIGNING_CERTIFICATES)
            ?: error("APK повреждён")
        @Suppress("DEPRECATION") val current = pm.getPackageInfo(activity.packageName, PackageManager.GET_SIGNING_CERTIFICATES)
        check(candidate.packageName == activity.packageName && candidate.longVersionCode == code && code > current.longVersionCode) { "Пакет или версия APK не совпадают" }
        val a = candidate.signingInfo?.apkContentsSigners?.map { it.toCharsString() }?.toSet().orEmpty()
        val b = current.signingInfo?.apkContentsSigners?.map { it.toCharsString() }?.toSet().orEmpty()
        check(a.isNotEmpty() && a == b) { "Подпись APK отличается от установленного Argus" }
    }

    fun offerInstall() {
        val file = pendingInstall ?: return
        if (activity.isFinishing || activity.isDestroyed) return
        if (!activity.packageManager.canRequestPackageInstalls()) {
            AlertDialog.Builder(activity).setTitle("Разрешение Android")
                .setMessage("Для обновления разрешите Argus устанавливать приложения. Затем нажмите строку обновлений ещё раз.")
                .setNegativeButton("Позже", null).setPositiveButton("Открыть настройку") { _, _ ->
                    activity.startActivity(Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES, Uri.parse("package:${activity.packageName}")))
                }.show(); return
        }
        val uri = FileProvider.getUriForFile(activity, "${activity.packageName}.files", file)
        activity.startActivity(Intent(Intent.ACTION_VIEW).setDataAndType(uri, "application/vnd.android.package-archive")
            .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION))
        status("APK проверен · подтвердите установку в Android")
        pendingInstall = null
    }

    fun onTap() { if (pendingInstall != null) offerInstall() else check(true) }
    fun close() { worker.shutdown() }
    private fun trustedAsset(value: String): String {
        val u = Uri.parse(value)
        check(u.scheme == "https" && u.host == "github.com" && u.path.orEmpty().startsWith("/$repository/releases/download/") && u.userInfo == null) { "Неверный адрес APK" }
        return value
    }
    private fun open(value: String): HttpURLConnection {
        val c = URL(value).openConnection() as HttpURLConnection
        c.connectTimeout = 8_000; c.readTimeout = 20_000
        c.setRequestProperty("Accept", "application/vnd.github+json")
        c.setRequestProperty("User-Agent", "Argus-Connect/${BuildConfig.VERSION_NAME}")
        check(c.responseCode == 200) { val code = c.responseCode; c.disconnect(); "GitHub HTTP $code" }
        check(c.url.protocol == "https") { c.disconnect(); "Небезопасный адрес загрузки" }
        return c
    }
    private fun get(value: String, limit: Int): ByteArray {
        val c = open(value)
        return try { c.inputStream.use { input -> val output = java.io.ByteArrayOutputStream(); val buffer = ByteArray(8192); while (true) { val n = input.read(buffer); if (n < 0) break; check(output.size() + n <= limit) { "Ответ GitHub слишком большой" }; output.write(buffer, 0, n) }; val result = output.toByteArray(); check(result.size <= limit) { "Ответ GitHub слишком большой" }; result } }
        finally { c.disconnect() }
    }
}
